# Migrating the search index

A version that changes how resources are indexed works on a new index and
leaves the old one untouched. The service starts normally, but the new index is
empty: search finds nothing until it is filled. The old index stays around
until you remove it.

## v7.x.x to v8.x.x

> [!IMPORTANT]
> On 8.0.x and 8.1.x, `opencloud search index` needs `--insecure` in the
> default setup, where the search service runs gRPC without TLS: add it to the
> commands below. Later releases don't need it.

### OpenSearch

Fill the new index by indexing all spaces again:

```shell
# the service keeps running while it happens
opencloud search index --all-spaces
```

Once the new index is filled, every index but the one with the highest
`-v<N>` suffix can go (indexes up to 7.4 have no suffix):

```shell
curl "https://os.example.com:9200/_cat/indices/opencloud-resource*"
curl -X DELETE "https://os.example.com:9200/opencloud-resource"
```

### bleve

The new index is a directory next to the old `bleve` one, both in
`$OC_BASE_DATA_PATH/search` by default (`SEARCH_ENGINE_BLEVE_DATA_PATH`). A
bleve index cannot be copied, index all spaces again:

```shell
opencloud search index --all-spaces
```

Once the new index is filled, every directory but the one with the highest
`bleve-v<N>` suffix can go (directories up to 7.4 have no suffix):

```shell
rm -r "$OC_BASE_DATA_PATH/search/bleve"
```

## Changing the content analyzer

An index keeps the content analyzer it was created with, and the service does
not start if `SEARCH_CONTENT_ANALYZER` differs. Point the service at a new
index (`SEARCH_ENGINE_OPEN_SEARCH_RESOURCE_INDEX_NAME` or
`SEARCH_ENGINE_BLEVE_DATA_PATH`), restart it, run
`opencloud search index --all-spaces --force-rescan`, then remove the old index
with the commands above, using its old name or path. The same applies after
changing the file in `SEARCH_ENGINE_OPEN_SEARCH_CONTENT_ANALYSIS_FILE`: it is
read and validated on every start, but only applied when the index is created,
and a change is not detected.
