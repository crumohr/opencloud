package opensearch_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	opensearchgo "github.com/opensearch-project/opensearch-go/v4"
	opensearchgoAPI "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/opencloud-eu/opencloud/pkg/log"
	searchService "github.com/opencloud-eu/opencloud/protogen/gen/opencloud/services/search/v0"
	"github.com/opencloud-eu/opencloud/services/search/internal/opensearchtest"
	searchmapping "github.com/opencloud-eu/opencloud/services/search/pkg/mapping"
	"github.com/opencloud-eu/opencloud/services/search/pkg/opensearch"
	"github.com/opencloud-eu/opencloud/services/search/pkg/search"
)

// TestVersionedIndexName guards that the index name and the generator identity
// carry the same schema version.
func TestVersionedIndexName(t *testing.T) {
	require.Equal(t,
		fmt.Sprintf("opencloud-resource-v%d", search.SchemaVersion),
		opensearch.VersionedIndexName("opencloud-resource"),
	)
	require.Equal(t,
		fmt.Sprintf("resource_v%d", search.SchemaVersion),
		string(opensearch.IndexManagerLatest),
	)
}

// A diff here means the generated OpenSearch index definition changed: new
// indexes get the new shape, existing ones answer to the startup classifier.
// Update the golden deliberately (UPDATE_GOLDEN=1); a breaking change needs a
// search.SchemaVersion bump.
func TestGoldenMapping(t *testing.T) {
	var pretty bytes.Buffer
	require.NoError(t, json.Indent(&pretty, []byte(opensearch.IndexManagerLatest.String()), "", "  "))
	pretty.WriteByte('\n')

	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.WriteFile("testdata/resource.golden.json", pretty.Bytes(), 0o644))
	}

	goldenB, err := os.ReadFile("testdata/resource.golden.json")
	require.NoError(t, err)

	var got, golden map[string]any
	require.NoError(t, json.Unmarshal(pretty.Bytes(), &got))
	require.NoError(t, json.Unmarshal(goldenB, &golden))
	require.Equal(t, golden, got, goldenAdvice(golden, got, "mappings.properties", "settings.analysis"))
}

// goldenAdvice classifies a golden diff so the failure says whether the change
// is additive (regenerate with UPDATE_GOLDEN=1) or breaking (bump
// search.SchemaVersion too).
func goldenAdvice(golden, got map[string]any, propsPath, analysisPath string) string {
	dig := func(m map[string]any, path string) map[string]any {
		for _, k := range strings.Split(path, ".") {
			m, _ = m[k].(map[string]any)
		}
		return m
	}
	c := searchmapping.Classify(dig(golden, propsPath), dig(got, propsPath), nil)
	if !reflect.DeepEqual(dig(golden, analysisPath), dig(got, analysisPath)) {
		c.AddBreaking("the analysis settings changed")
	}
	switch c.Verdict {
	case searchmapping.VerdictAdditive:
		return fmt.Sprintf("additive schema change (new fields: %v): regenerate the golden with UPDATE_GOLDEN=1, no SchemaVersion bump needed", c.NewFields)
	case searchmapping.VerdictBreaking:
		return fmt.Sprintf("breaking schema change (%v): regenerate the golden with UPDATE_GOLDEN=1 and bump search.SchemaVersion", c.Reasons)
	}
	return "the schema changed outside the classified tree: regenerate the golden with UPDATE_GOLDEN=1"
}

func TestContentAnalyzer(t *testing.T) {
	t.Run("words keeps the default index definition", func(t *testing.T) {
		b, err := opensearch.IndexManagerLatest.Build(opensearch.WithContentAnalyzer("words"))
		require.NoError(t, err)
		require.Equal(t, opensearch.IndexManagerLatest.String(), string(b))
	})

	t.Run("finds another form of a word in the contents with german only", func(t *testing.T) {
		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		for analyzer, matches := range map[string]int{"words": 0, "german": 1} {
			name := "opencloud-test-content-" + analyzer
			tc.Require.IndicesReset([]string{opensearch.VersionedIndexName(name)})
			backend, err := opensearch.NewBackend(t.Context(), name, tc.Client(), log.NopLogger(), opensearch.WithContentAnalyzer(analyzer))
			require.NoError(t, err)
			doc := opensearchtest.Testdata.Resources.File
			doc.Content = "Die Verträge"
			require.NoError(t, backend.Upsert(doc.ID, doc))
			tc.Require.IndicesRefresh([]string{opensearch.VersionedIndexName(name)}, nil)

			resp, err := backend.Search(t.Context(), &searchService.SearchIndexRequest{Query: "content:Vertrag"})
			require.NoError(t, err)
			require.Len(t, resp.Matches, matches, analyzer)
		}
	})

	t.Run("refuses another analyzer on an existing index", func(t *testing.T) {
		indexName := "opencloud-test-resource"
		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		tc.Require.IndicesReset([]string{indexName})
		tc.Require.IndicesCreate(indexName, strings.NewReader(opensearch.IndexManagerLatest.String()))

		err := opensearch.IndexManagerLatest.Apply(t.Context(), indexName, tc.Client(), log.NopLogger(), opensearch.WithContentAnalyzer("german"))
		require.ErrorIs(t, err, opensearch.ErrManualActionRequired)
	})
}

func TestContentAnalysis(t *testing.T) {
	const definitions = `{
		"filter": {"content_compounds": {"type": "dictionary_decompounder", "word_list": ["reise", "kosten"], "min_subword_size": 4, "only_longest_match": false}},
		"analyzer": {"content_german": {"type": "custom", "tokenizer": "standard", "filter": ["lowercase", "content_compounds", "german_normalization", "german_stem"]}}
	}`
	withDefinitions := func(definitions string) []opensearch.IndexOption {
		return []opensearch.IndexOption{opensearch.WithContentAnalyzer("content_german"), opensearch.WithContentAnalysis([]byte(definitions))}
	}

	t.Run("refuses definitions it cannot add", func(t *testing.T) {
		for definitions, wantErr := range map[string]string{
			`null`:               "not a JSON object",
			`{} {}`:              "trailing content",
			`{"similarity": {}}`: `cannot define section "similarity"`,
			`{"tokenizer": {"standard": {"type": "keyword"}}}`:       `cannot define tokenizer "standard"`,
			`{"tokenizer": {"content_t.type": {"type": "keyword"}}}`: `cannot define tokenizer "content_t.type"`,
			`{"filter": {"content_f": 1}}`:                           `cannot define filter "content_f"`,
			`{"analyzer": {"content_other": {"type": "german"}}}`:    `unknown content analyzer "content_german"`,
		} {
			_, err := opensearch.IndexManagerLatest.Build(withDefinitions(definitions)...)
			require.ErrorContains(t, err, wantErr, definitions)
		}
	})

	t.Run("finds a part of a compound word", func(t *testing.T) {
		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		name := "opencloud-test-content-analysis"
		tc.Require.IndicesReset([]string{opensearch.VersionedIndexName(name)})
		backend, err := opensearch.NewBackend(t.Context(), name, tc.Client(), log.NopLogger(), withDefinitions(definitions)...)
		require.NoError(t, err)
		doc := opensearchtest.Testdata.Resources.File
		doc.Content = "Reisekosten"
		require.NoError(t, backend.Upsert(doc.ID, doc))
		tc.Require.IndicesRefresh([]string{opensearch.VersionedIndexName(name)}, nil)

		resp, err := backend.Search(t.Context(), &searchService.SearchIndexRequest{Query: "content:Reise"})
		require.NoError(t, err)
		require.Len(t, resp.Matches, 1)

		// the definitions only apply when the index is created
		changed := strings.Replace(definitions, `"kosten"`, `"kosten", "dienst"`, 1)
		require.NoError(t, opensearch.IndexManagerLatest.Apply(t.Context(), opensearch.VersionedIndexName(name), tc.Client(), log.NopLogger(), withDefinitions(definitions)...))
		require.NoError(t, opensearch.IndexManagerLatest.Apply(t.Context(), opensearch.VersionedIndexName(name), tc.Client(), log.NopLogger(), withDefinitions(changed)...))

		// and an index created with a file opens without one if its analyzer is built in
		tc.Require.IndicesReset([]string{"opencloud-test-resource"})
		require.NoError(t, opensearch.IndexManagerLatest.Apply(t.Context(), "opencloud-test-resource", tc.Client(), log.NopLogger(), opensearch.WithContentAnalyzer("german"), opensearch.WithContentAnalysis([]byte(definitions))))
		require.NoError(t, opensearch.IndexManagerLatest.Apply(t.Context(), "opencloud-test-resource", tc.Client(), log.NopLogger(), opensearch.WithContentAnalyzer("german")))
	})

	t.Run("still refuses a changed built-in definition", func(t *testing.T) {
		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		indexName := "opencloud-test-resource"
		tc.Require.IndicesReset([]string{indexName})
		b, err := opensearch.IndexManagerLatest.Build(withDefinitions(definitions)...)
		require.NoError(t, err)
		drifted, err := sjson.SetBytes(b, "settings.analysis.analyzer.words.filter", []string{"lowercase", "asciifolding"})
		require.NoError(t, err)
		tc.Require.IndicesCreate(indexName, bytes.NewReader(drifted))

		err = opensearch.IndexManagerLatest.Apply(t.Context(), indexName, tc.Client(), log.NopLogger(), withDefinitions(definitions)...)
		require.ErrorIs(t, err, opensearch.ErrManualActionRequired)
		require.ErrorContains(t, err, "settings.analysis changed")
		require.NotContains(t, err.Error(), "content analyzer")
	})

	t.Run("refuses an index with an unprefixed extra definition", func(t *testing.T) {
		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		indexName := "opencloud-test-resource"
		tc.Require.IndicesReset([]string{indexName})
		body, err := sjson.Set(opensearch.IndexManagerLatest.String(), "settings.analysis.tokenizer.standard", map[string]string{"type": "keyword"})
		require.NoError(t, err)
		tc.Require.IndicesCreate(indexName, strings.NewReader(body))

		err = opensearch.IndexManagerLatest.Apply(t.Context(), indexName, tc.Client(), log.NopLogger(), opensearch.WithContentAnalysis([]byte(`{}`)))
		require.ErrorIs(t, err, opensearch.ErrManualActionRequired)
		require.ErrorContains(t, err, `settings.analysis changed`)
		require.ErrorContains(t, err, `"standard":{"type":"keyword"}`)
	})
}

func TestIndexManager(t *testing.T) {
	t.Run("index plausibility", func(t *testing.T) {
		tests := []opensearchtest.TableTest[opensearch.IndexManager, struct{}]{
			{
				Name: "empty",
				Got:  opensearch.IndexManagerLatest,
			},
		}
		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)

		for _, test := range tests {
			t.Run(test.Name, func(t *testing.T) {
				indexName := "opencloud-test-resource"
				tc.Require.IndicesReset([]string{indexName})

				body, err := test.Got.MarshalJSON()
				require.NoError(t, err)
				require.NotEmpty(t, body)
				require.NotEmpty(t, test.Got.String())
				require.JSONEq(t, test.Got.String(), string(body))
				require.NoError(t, test.Got.Apply(t.Context(), indexName, tc.Client(), log.NopLogger()))
			})
		}
	})

	t.Run("does not create index if it already exists and is up to date", func(t *testing.T) {
		indexManager := opensearch.IndexManagerLatest
		indexName := "opencloud-test-resource"

		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		tc.Require.IndicesReset([]string{indexName})
		tc.Require.IndicesCreate(indexName, strings.NewReader(indexManager.String()))

		require.NoError(t, indexManager.Apply(t.Context(), indexName, tc.Client(), log.NopLogger()))
	})

	t.Run("fails when the analysis settings drift", func(t *testing.T) {
		indexManager := opensearch.IndexManagerLatest
		indexName := "opencloud-test-resource"

		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		tc.Require.IndicesReset([]string{indexName})

		body, err := sjson.Set(indexManager.String(), "settings.analysis.analyzer.lowercaseKeyword.tokenizer", "standard")
		require.NoError(t, err)
		tc.Require.IndicesCreate(indexName, strings.NewReader(body))

		require.ErrorIs(t, indexManager.Apply(t.Context(), indexName, tc.Client(), log.NopLogger()), opensearch.ErrManualActionRequired)
	})

	t.Run("tolerates replica drift", func(t *testing.T) {
		indexManager := opensearch.IndexManagerLatest
		indexName := "opencloud-test-resource"

		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		tc.Require.IndicesReset([]string{indexName})

		body, err := sjson.Set(indexManager.String(), "settings.number_of_replicas", "2")
		require.NoError(t, err)
		tc.Require.IndicesCreate(indexName, strings.NewReader(body))

		require.NoError(t, indexManager.Apply(t.Context(), indexName, tc.Client(), log.NopLogger()))
	})

	t.Run("tolerates shard drift", func(t *testing.T) {
		indexManager := opensearch.IndexManagerLatest
		indexName := "opencloud-test-resource"

		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		tc.Require.IndicesReset([]string{indexName})

		body, err := sjson.Set(indexManager.String(), "settings.number_of_shards", "2")
		require.NoError(t, err)
		tc.Require.IndicesCreate(indexName, strings.NewReader(body))

		require.NoError(t, indexManager.Apply(t.Context(), indexName, tc.Client(), log.NopLogger()))
	})

	t.Run("is idempotent", func(t *testing.T) {
		indexManager := opensearch.IndexManagerLatest
		indexName := "opencloud-test-resource"

		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		tc.Require.IndicesReset([]string{indexName})

		require.NoError(t, indexManager.Apply(t.Context(), indexName, tc.Client(), log.NopLogger()))
		require.NoError(t, indexManager.Apply(t.Context(), indexName, tc.Client(), log.NopLogger()))
	})

	t.Run("adds a new field to an existing index in place", func(t *testing.T) {
		indexManager := opensearch.IndexManagerLatest
		indexName := "opencloud-test-resource"

		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		tc.Require.IndicesReset([]string{indexName})

		body, err := sjson.Delete(indexManager.String(), "mappings.properties.Title")
		require.NoError(t, err)
		tc.Require.IndicesCreate(indexName, strings.NewReader(body))

		require.NoError(t, indexManager.Apply(t.Context(), indexName, tc.Client(), log.NopLogger()))

		resp, err := tc.Client().Indices.Mapping.Get(t.Context(), &opensearchgoAPI.MappingGetReq{Indices: []string{indexName}})
		require.NoError(t, err)
		require.True(t, gjson.GetBytes(resp.GetIndices()[indexName].Mappings, "properties.Title").Exists())
	})

	t.Run("adds a new nested field to an existing index in place", func(t *testing.T) {
		indexManager := opensearch.IndexManagerLatest
		indexName := "opencloud-test-resource"

		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		tc.Require.IndicesReset([]string{indexName})

		body, err := sjson.Delete(indexManager.String(), "mappings.properties.photo.properties.cameraMake")
		require.NoError(t, err)
		tc.Require.IndicesCreate(indexName, strings.NewReader(body))

		require.NoError(t, indexManager.Apply(t.Context(), indexName, tc.Client(), log.NopLogger()))

		resp, err := tc.Client().Indices.Mapping.Get(t.Context(), &opensearchgoAPI.MappingGetReq{Indices: []string{indexName}})
		require.NoError(t, err)
		require.True(t, gjson.GetBytes(resp.GetIndices()[indexName].Mappings, "properties.photo.properties.cameraMake").Exists())
	})

	t.Run("fails when an existing field changed its definition", func(t *testing.T) {
		indexManager := opensearch.IndexManagerLatest
		indexName := "opencloud-test-resource"

		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		tc.Require.IndicesReset([]string{indexName})

		body, err := sjson.Set(indexManager.String(), "mappings.properties.Deleted.type", "keyword")
		require.NoError(t, err)
		tc.Require.IndicesCreate(indexName, strings.NewReader(body))

		require.ErrorIs(t, indexManager.Apply(t.Context(), indexName, tc.Client(), log.NopLogger()), opensearch.ErrManualActionRequired)
	})

	t.Run("fails when the index contains a field the code schema does not know", func(t *testing.T) {
		indexManager := opensearch.IndexManagerLatest
		indexName := "opencloud-test-resource"

		tc := opensearchtest.NewDefaultTestClient(t, defaultConfig.Engine.OpenSearch.Client)
		tc.Require.IndicesReset([]string{indexName})

		body, err := sjson.Set(indexManager.String(), "mappings.properties.legacyField.type", "keyword")
		require.NoError(t, err)
		tc.Require.IndicesCreate(indexName, strings.NewReader(body))

		require.ErrorIs(t, indexManager.Apply(t.Context(), indexName, tc.Client(), log.NopLogger()), opensearch.ErrManualActionRequired)
	})

	t.Run("transport errors do not demand manual action", func(t *testing.T) {
		client, err := opensearchgoAPI.NewClient(opensearchgoAPI.Config{
			Client: opensearchgo.Config{
				Addresses: []string{"http://localhost:1025"},
			},
		})
		require.NoError(t, err)

		err = opensearch.IndexManagerLatest.Apply(t.Context(), "opencloud-test-resource", client, log.NopLogger())
		require.Error(t, err)
		require.NotErrorIs(t, err, opensearch.ErrManualActionRequired)
	})
}
