# REMP Elasticsearch Output Plugin

Writes REMP Beam events (pageviews, timespent, progress, impressions, events,
commerce, entities, ...) to Elasticsearch 8. Compared to the upstream
`outputs.elasticsearch` plugin it

- writes tags and fields as top-level document fields and the timestamp to
  `time` (RFC 3339, second precision),
- merges the JSON object from the `_json` string field into the document (the
  REMP tracker uses it to keep types line protocol can't carry),
- converts tag values `"1"`/`"0"` to booleans (legacy, before `_json`),
- uses a field of the event as the document ID (`id_field`),
- can upsert documents: overwrite selected fields (`updated_fields`) or add
  values to array fields (`added_fields`) of an existing document,
- can filter fields, including the ones packed in `_json` (`field_whitelist`).

## Configuration

See [sample.conf](sample.conf).

## Write modes

| Configuration | Bulk operation | Effect |
|---|---|---|
| neither `updated_fields` nor `added_fields` | `index` | Creates the document, or replaces it when `id_field` points to an existing one. |
| `updated_fields` | `update` with `upsert` | New ID: the whole document is created. Existing ID: only the listed fields are overwritten; a listed field missing in the event is set to `null`. |
| `added_fields` | `update` with `upsert` | New ID: the whole document is created. Existing ID: values of the listed fields are added to the stored arrays without duplicates (scalars count as one-element arrays). Events with none of the listed fields are skipped. |

Updates are retried by Elasticsearch on version conflicts
(`retry_on_conflict=3`), which happen when several Telegraf instances update
the same document.

## Error handling

All metrics of a flush are sent in one bulk request.

- **Dropped and logged:** metrics that can't be turned into a document (no or
  non-string ID, invalid `_json`, NaN/Inf values) and documents Elasticsearch
  refuses with status 400 because of their content (mapping conflict, script
  error, ...). They would fail the same way on every retry and block the
  output.
- **Kept and retried on the next flush:** everything else. This includes 400
  errors caused by the target index (no write index for an alias, shard limit
  reached when creating an index, invalid index name), 403 (write block,
  missing privileges), 404 (missing index), 409, 429, 5xx and failed requests.
  With `inputs.kafka_consumer` the data waits in Kafka until the problem is
  fixed.

At most five dropped documents are logged per flush in detail; the total
count is in the "Error writing to outputs.remp_elasticsearch" message.

## Deprecated and removed options

Still accepted so that existing configuration files load; Telegraf logs a
deprecation warning when they are set:

- `health_check_interval`: no effect.
- `manage_template`, `template_name`, `overwrite_template`: no effect, manage
  index templates in Elasticsearch.
- `ssl_ca`, `ssl_cert`, `ssl_key`: aliases of `tls_ca`, `tls_cert`, `tls_key`.

Removed (Telegraf refuses to start when they are set): `incremented_fields`
(it never incremented existing documents), `update_query_field`, `tag_keys`
and `type_name` (never had any effect).
