package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"

	opensearchgo "github.com/opensearch-project/opensearch-go/v4"
	opensearchgoAPI "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/tidwall/gjson"

	"github.com/opencloud-eu/opencloud/pkg/log"
	searchmapping "github.com/opencloud-eu/opencloud/services/search/pkg/mapping"
	"github.com/opencloud-eu/opencloud/services/search/pkg/search"
)

var (
	// ErrManualActionRequired is the shared sentinel, see the mapping package.
	ErrManualActionRequired = searchmapping.ErrManualActionRequired

	// IndexManagerLatest identifies the current resource mapping; its version is
	// derived from search.SchemaVersion so it never drifts from the index name.
	IndexManagerLatest = IndexManager(fmt.Sprintf("resource_v%d", search.SchemaVersion))
)

// VersionedIndexName suffixes the base index name with the schema version, e.g.
// "opencloud-resource" -> the name suffixed with the current schema version.
func VersionedIndexName(base string) string {
	return fmt.Sprintf("%s-v%d", base, search.SchemaVersion)
}

type IndexManager string

// IndexOption changes the index definition an IndexManager builds.
type IndexOption func(*indexOptions)

type indexOptions struct {
	contentAnalyzer string
	contentAnalysis []byte
}

// WithContentAnalyzer analyzes the Content field with the named analyzer.
func WithContentAnalyzer(name string) IndexOption {
	return func(o *indexOptions) { o.contentAnalyzer = name }
}

// contentAnalysisPrefix starts every name in the operator's analysis
// definitions, so they cannot replace or shadow any other definition.
const contentAnalysisPrefix = "content_"

// contentAnalysisSections are the sections of settings.analysis the
// operator's definitions may add to.
var contentAnalysisSections = strings.Fields("analyzer char_filter filter normalizer tokenizer")

// WithContentAnalysis adds analysis definitions, a JSON object shaped like
// settings.analysis, to the index settings.
func WithContentAnalysis(definitions []byte) IndexOption {
	return func(o *indexOptions) { o.contentAnalysis = definitions }
}

// indexGenerators dispatches each IndexManager variant to its builder.
var indexGenerators = map[IndexManager]func(indexOptions) ([]byte, error){
	IndexManagerLatest: buildResourceMapping,
}

func (m IndexManager) String() string {
	b, err := m.MarshalJSON()
	if err != nil {
		return ""
	}

	return string(b)
}

func (m IndexManager) MarshalJSON() ([]byte, error) {
	return m.Build()
}

// Build renders the index definition with the given options.
func (m IndexManager) Build(opts ...IndexOption) ([]byte, error) {
	gen, ok := indexGenerators[m]
	if !ok {
		return nil, fmt.Errorf("unknown index manager %q", string(m))
	}
	var o indexOptions
	for _, opt := range opts {
		opt(&o)
	}
	return gen(o)
}

// buildResourceMapping renders the OpenSearch index template for a
// search.Resource from the shared SearchFieldOverrides. OpenSearch-specific
// tweaks (wildcard MimeType, path_hierarchy Path) are applied on top.
func buildResourceMapping(o indexOptions) ([]byte, error) {
	resourceType := reflect.TypeFor[search.Resource]()
	overrides := maps.Clone(search.Resource{}.SearchFieldOverrides())
	mimeType := overrides["MimeType"]
	mimeType.Type = searchmapping.TypeWildcard
	overrides["MimeType"] = mimeType
	var definitions map[string]map[string]any
	if o.contentAnalysis != nil {
		decoder := json.NewDecoder(bytes.NewReader(o.contentAnalysis))
		decoder.UseNumber() // keep large numbers exact
		err := decoder.Decode(&definitions)
		_, next := decoder.Token()
		if err == nil && next != io.EOF {
			err = errors.New("trailing content after the JSON object")
		}
		switch {
		case errors.Is(err, io.EOF):
			return nil, errors.New("content analysis: the file is empty")
		case err != nil:
			return nil, fmt.Errorf("content analysis: %w", err)
		case definitions == nil:
			return nil, errors.New("content analysis: not a JSON object")
		}
		for section, named := range definitions {
			if named == nil || !slices.Contains(contentAnalysisSections, section) {
				return nil, fmt.Errorf("content analysis: cannot define section %q", section)
			}
			for name, definition := range named {
				// a dotted name would be read as a path into another setting
				if _, object := definition.(map[string]any); !object || !strings.HasPrefix(name, contentAnalysisPrefix) || strings.Contains(name, ".") {
					return nil, fmt.Errorf("content analysis: cannot define %s %q, names start with %q and contain no dot, definitions are objects", section, name, contentAnalysisPrefix)
				}
			}
		}
	}
	if _, defined := definitions["analyzer"][o.contentAnalyzer].(map[string]any); !defined {
		_, err := searchmapping.LookupContentAnalyzer(o.contentAnalyzer)
		if err != nil {
			return nil, err
		}
	}
	content := overrides["Content"]
	content.Analyzer = o.contentAnalyzer
	overrides["Content"] = content
	if err := searchmapping.Validate(resourceType, overrides); err != nil {
		return nil, err
	}
	props, err := searchmapping.OpenSearchBuildMapping(resourceType, overrides)
	if err != nil {
		return nil, err
	}

	index := map[string]any{
		"settings": map[string]any{
			"number_of_shards":   "1",
			"number_of_replicas": "1",
			"analysis": map[string]any{
				// path_hierarchy is case-preserving; casing lives in the value.
				"analyzer": map[string]any{
					searchmapping.PathAnalyzer: map[string]any{
						"type":      "custom",
						"tokenizer": "path_hierarchy",
					},
					// words: split into lowercased words, a dot is a word boundary
					// too so that "report" finds "Report.txt"; no stemming, a name
					// is not prose
					searchmapping.WordsAnalyzer: map[string]any{
						"type":        "custom",
						"char_filter": []string{"dot_to_space"},
						"tokenizer":   "standard",
						"filter":      []string{"lowercase"},
					},
				},
				"char_filter": map[string]any{
					"dot_to_space": map[string]any{
						"type":     "mapping",
						"mappings": []string{`. => \u0020`},
					},
				},
				"tokenizer": map[string]any{
					"path_hierarchy": map[string]any{"type": "path_hierarchy"},
				},
			},
		},
		"mappings": map[string]any{
			"properties": props,
		},
	}
	analysis, _ := index["settings"].(map[string]any)["analysis"].(map[string]any)
	for section, named := range definitions {
		merged, _ := analysis[section].(map[string]any)
		if merged == nil {
			merged = map[string]any{}
			analysis[section] = merged
		}
		maps.Copy(merged, named)
	}
	return json.Marshal(index)
}

// Apply ensures the index exists and matches the code schema: created if
// missing, otherwise reconciled via searchmapping.Reconcile (see osReconciler).
func (m IndexManager) Apply(ctx context.Context, name string, client *opensearchgoAPI.Client, logger log.Logger, opts ...IndexOption) error {
	localIndexB, err := m.Build(opts...)
	if err != nil {
		return fmt.Errorf("failed to marshal index %s: %w", name, err)
	}

	// Exists first: a pre-provisioned index must not require create privileges
	indicesExistsResp, err := client.Indices.Exists(ctx, opensearchgoAPI.IndicesExistsReq{
		Indices: []string{name},
	})
	switch {
	case indicesExistsResp != nil && indicesExistsResp.StatusCode == 404:
		createResp, createErr := client.Indices.Create(ctx, opensearchgoAPI.IndicesCreateReq{
			Index: name,
			Body:  bytes.NewReader(localIndexB),
		})
		var structErr *opensearchgo.StructError
		switch {
		case createErr == nil && createResp.Acknowledged:
			searchmapping.LogNewIndexCreated(logger, name)
			return nil
		case createErr == nil:
			return fmt.Errorf("failed to create index %s: not acknowledged", name)
		case !errors.As(createErr, &structErr) || structErr.Err.Type != "resource_already_exists_exception":
			// transport errors, disk-full etc. stay plain fatal, the restart policy retries
			return fmt.Errorf("failed to create index %s: %w", name, createErr)
		}
		// lost the creation race to another instance, compare against its index
	case err != nil:
		return fmt.Errorf("failed to check if index %s exists: %w", name, err)
	case indicesExistsResp == nil:
		return fmt.Errorf("indicesExistsResp is nil for index %s", name)
	}

	// the index exists: reconcile its schema through the shared verdict flow
	resp, err := client.Indices.Get(ctx, opensearchgoAPI.IndicesGetReq{
		Indices: []string{name},
	})
	if err != nil {
		return fmt.Errorf("failed to get index %s: %w", name, err)
	}

	remoteIndex, ok := (*resp.IndicesGetRespData)[name]
	if !ok {
		return fmt.Errorf("index %s not found in response", name)
	}
	remoteIndexB, err := json.Marshal(remoteIndex)
	if err != nil {
		return fmt.Errorf("failed to marshal index %s: %w", name, err)
	}

	var o indexOptions
	for _, opt := range opts {
		opt(&o)
	}
	r := &osReconciler{
		ctx:    ctx,
		name:   name,
		client: client,
		local:  gjson.ParseBytes(localIndexB),
		remote: gjson.ParseBytes(remoteIndexB),
	}
	if o.contentAnalysis != nil {
		logger.Info().Str("index", name).Msg("the content analysis file is only applied when an index is created, this index keeps the definitions it was created with")
	}
	_, err = searchmapping.Reconcile(name, r, logger)
	return err
}

// osReconciler adapts an existing OpenSearch index to searchmapping.SchemaReconciler.
type osReconciler struct {
	ctx    context.Context
	name   string
	client *opensearchgoAPI.Client
	local  gjson.Result
	remote gjson.Result
}

func (r *osReconciler) Classify() (searchmapping.Classification, error) {
	const contentAnalyzer = "mappings.properties.Content.analyzer"
	configured := r.local.Get(contentAnalyzer).String()
	if indexed := r.remote.Get(contentAnalyzer); indexed.Exists() && indexed.String() != configured {
		return searchmapping.Classification{}, searchmapping.ContentAnalyzerChangedError(r.name, indexed.String(), configured)
	}

	// Only the analysis settings affect indexing correctness; shard/replica
	// counts and other operational knobs are the operator's to tune (and a
	// pre-provisioned index's to own), so they are not compared.
	var reasons []string
	// the operator's definitions only apply when the index is created
	lv := withoutContentAnalysis(r.local.Get("settings.analysis").Raw)
	rv := withoutContentAnalysis(r.remote.Get("settings.index.analysis").Raw)
	if !jsonEqual(lv, rv) {
		reasons = append(reasons, fmt.Sprintf("settings.analysis changed: index %s, code %s", rawOrUnset(rv), rawOrUnset(lv)))
	}

	classification := searchmapping.Classify(
		propertiesMap(r.remote.Get("mappings.properties").Raw),
		propertiesMap(r.local.Get("mappings.properties").Raw),
		nil,
	)
	classification.AddBreaking(reasons...)
	return classification, nil
}

// ApplyAdditive puts the full code properties (only additions, per the
// classifier). The PUT is atomic, so persisted is true only on success.
func (r *osReconciler) ApplyAdditive() (bool, error) {
	putResp, err := r.client.Indices.Mapping.Put(r.ctx, opensearchgoAPI.MappingPutReq{
		Indices: []string{r.name},
		Body:    strings.NewReader(r.local.Get("mappings").Raw),
	})
	var putErr *opensearchgo.StructError
	switch {
	case err != nil && errors.As(err, &putErr) && putErr.Err.Type == "illegal_argument_exception" &&
		(strings.Contains(putErr.Err.Reason, "cannot be changed") || strings.Contains(putErr.Err.Reason, "Cannot update parameter")):
		// backstop, should be unreachable after the classification above
		return false, searchmapping.ManualActionRequiredError(r.name, []string{putErr.Err.Reason})
	case err != nil:
		return false, fmt.Errorf("failed to update mapping of index %s: %w", r.name, err)
	case !putResp.Acknowledged:
		return false, fmt.Errorf("failed to update mapping of index %s: not acknowledged", r.name)
	}
	return true, nil
}

// withoutContentAnalysis drops the operator's prefixed definitions, and the
// sections left empty, from a raw settings.analysis.
func withoutContentAnalysis(raw string) string {
	var analysis map[string]map[string]any
	err := json.Unmarshal([]byte(raw), &analysis)
	if err != nil {
		return raw
	}
	for _, section := range contentAnalysisSections {
		for name := range analysis[section] {
			if strings.HasPrefix(name, contentAnalysisPrefix) {
				delete(analysis[section], name)
			}
		}
		if len(analysis[section]) == 0 {
			delete(analysis, section)
		}
	}
	b, err := json.Marshal(analysis)
	if err != nil {
		return raw
	}
	return string(b)
}

// jsonEqual reports whether two raw JSON values are deeply equal. A missing
// gjson path is an empty string, so two unset values compare equal.
func jsonEqual(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// propertiesMap parses a raw mappings.properties object into a map. Missing,
// empty, null or malformed input yields an empty (non-nil) map, which
// classifies as purely additive.
func propertiesMap(raw string) map[string]any {
	props := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &props); err != nil || props == nil {
		return map[string]any{}
	}
	return props
}

func rawOrUnset(raw string) string {
	if raw == "" {
		return "(unset)"
	}
	return raw
}
