package remp_elasticsearch

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/elastic/go-elasticsearch/v8"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/internal"
	common_tls "github.com/influxdata/telegraf/plugins/common/tls"
	"github.com/influxdata/telegraf/plugins/outputs"
)

//go:embed sample.conf
var sampleConfig string

const (
	// jsonField carries a JSON-encoded object produced by the REMP tracker. Line
	// protocol can't express nested values or keep all types, so the tracker
	// packs them here and the plugin merges the object into the document.
	jsonField = "_json"

	// Overwrites the given fields of an existing document.
	updateScript = `for (e in params.fields.entrySet()) { ctx._source[e.getKey()] = e.getValue(); }`

	// Adds the given values to array fields of an existing document, keeping
	// every value only once. Scalars on either side are treated as one-element
	// arrays.
	addScript = `for (e in params.fields.entrySet()) {
  Set values = new LinkedHashSet();
  def current = ctx._source[e.getKey()];
  if (current instanceof Collection) { values.addAll(current); } else if (current != null) { values.add(current); }
  def incoming = e.getValue();
  if (incoming instanceof Collection) { values.addAll(incoming); } else if (incoming != null) { values.add(incoming); }
  ctx._source[e.getKey()] = new ArrayList(values);
}`

	// Concurrent updates of one document (several Telegraf instances in the
	// consumer group) are retried by Elasticsearch this many times.
	retryOnConflict = 3

	// Number of rejected documents logged in detail per batch.
	maxLoggedRejects = 5
)

// protectedFields are never removed by field_whitelist; REMP queries and
// aggregations depend on them.
var protectedFields = map[string]bool{
	"category":         true,
	"action":           true,
	"browser_id":       true,
	"user_id":          true,
	"article_id":       true,
	"remp_pageview_id": true,
	"remp_session_id":  true,
	"url":              true,
	"time":             true,
}

type RempElasticsearch struct {
	URLs            []string        `toml:"urls"`
	Timeout         config.Duration `toml:"timeout"`
	EnableSniffer   bool            `toml:"enable_sniffer"`
	Username        config.Secret   `toml:"username"`
	Password        config.Secret   `toml:"password"`
	IndexName       string          `toml:"index_name"`
	DefaultTagValue string          `toml:"default_tag_value"`
	IDField         string          `toml:"id_field"`
	UpdatedFields   []string        `toml:"updated_fields"`
	AddedFields     []string        `toml:"added_fields"`
	FieldWhitelist  []string        `toml:"field_whitelist"`
	common_tls.ClientConfig

	// Options of the olivere/elastic based plugin, accepted so that existing
	// configurations keep loading. Telegraf warns when they are set.
	HealthCheckInterval config.Duration `toml:"health_check_interval" deprecated:"1.40.3;has no effect, unavailable nodes are retried automatically"`
	ManageTemplate      bool            `toml:"manage_template" deprecated:"1.40.3;has no effect, manage index templates in Elasticsearch"`
	TemplateName        string          `toml:"template_name" deprecated:"1.40.3;has no effect, manage index templates in Elasticsearch"`
	OverwriteTemplate   bool            `toml:"overwrite_template" deprecated:"1.40.3;has no effect, manage index templates in Elasticsearch"`
	SSLCA               string          `toml:"ssl_ca" deprecated:"1.40.3;use 'tls_ca' instead"`
	SSLCert             string          `toml:"ssl_cert" deprecated:"1.40.3;use 'tls_cert' instead"`
	SSLKey              string          `toml:"ssl_key" deprecated:"1.40.3;use 'tls_key' instead"`

	Log telegraf.Logger `toml:"-"`

	client       *elasticsearch.Client
	tagPatterns  []tagPattern
	hasDates     bool
	whitelist    map[string]bool
	scriptSource string
	scriptFields []string
}

type tagPattern struct {
	placeholder string // e.g. "{{ host }}"
	key         string // e.g. "host"
}

// action is the metadata line of a bulk request item.
type action struct {
	Index  *actionMeta `json:"index,omitempty"`
	Update *actionMeta `json:"update,omitempty"`
}

type actionMeta struct {
	Index           string `json:"_index"`
	ID              string `json:"_id,omitempty"`
	RetryOnConflict int    `json:"retry_on_conflict,omitempty"`
}

type updateBody struct {
	Script script         `json:"script"`
	Upsert map[string]any `json:"upsert"`
}

type script struct {
	Source string         `json:"source"`
	Lang   string         `json:"lang"`
	Params map[string]any `json:"params"`
}

type bulkItemResp struct {
	ID     string         `json:"_id"`
	Status int            `json:"status"`
	Error  *bulkItemError `json:"error"`
}

type bulkItemError struct {
	Type     string         `json:"type"`
	Reason   string         `json:"reason"`
	CausedBy *bulkItemError `json:"caused_by"`
}

func (e *bulkItemError) String() string {
	if e == nil {
		return "unknown error"
	}
	s := e.Type + ": " + e.Reason
	if e.CausedBy != nil {
		s += " (caused by " + e.CausedBy.String() + ")"
	}
	return s
}

func (*RempElasticsearch) SampleConfig() string {
	return sampleConfig
}

func (a *RempElasticsearch) Init() error {
	if len(a.URLs) == 0 {
		return errors.New("no urls defined")
	}
	if a.IndexName == "" {
		return errors.New("index_name is not defined")
	}
	if len(a.UpdatedFields) > 0 && len(a.AddedFields) > 0 {
		return errors.New("updated_fields and added_fields cannot be used together; use two outputs")
	}
	if (len(a.UpdatedFields) > 0 || len(a.AddedFields) > 0) && a.IDField == "" {
		return errors.New("id_field is required when updated_fields or added_fields is set")
	}

	// Deprecated ssl_* options; tls_* wins when both are set.
	if a.TLSCA == "" {
		a.TLSCA = a.SSLCA
	}
	if a.TLSCert == "" {
		a.TLSCert = a.SSLCert
	}
	if a.TLSKey == "" {
		a.TLSKey = a.SSLKey
	}

	if a.Timeout <= 0 {
		a.Timeout = config.Duration(5 * time.Second)
	}

	a.hasDates = strings.Contains(a.IndexName, "%")
	a.tagPatterns = parseTagPatterns(a.IndexName)

	if len(a.FieldWhitelist) > 0 {
		a.whitelist = make(map[string]bool, len(a.FieldWhitelist)+len(protectedFields))
		for k := range protectedFields {
			a.whitelist[k] = true
		}
		for _, k := range a.FieldWhitelist {
			a.whitelist[k] = true
		}
	}

	switch {
	case len(a.UpdatedFields) > 0:
		a.scriptSource, a.scriptFields = updateScript, a.UpdatedFields
	case len(a.AddedFields) > 0:
		a.scriptSource, a.scriptFields = addScript, a.AddedFields
	}

	return nil
}

func (a *RempElasticsearch) Connect() error {
	tlsCfg, err := a.ClientConfig.TLSConfig()
	if err != nil {
		return fmt.Errorf("creating TLS config: %w", err)
	}

	// Proxy is left unset on purpose: the olivere/elastic based plugin didn't
	// honour HTTP_PROXY either and silently starting to do so could reroute
	// existing installations.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = tlsCfg

	cfg := elasticsearch.Config{
		Addresses: a.URLs,
		Transport: transport,
		// Telegraf retries the batch on the next flush. Retrying inside the
		// client as well could duplicate documents without id_field when a
		// response is lost. A failed node is still skipped by the next request.
		DisableRetry: true,
	}
	if a.EnableSniffer {
		cfg.DiscoverNodesOnStart = true
		cfg.DiscoverNodesInterval = 15 * time.Minute
	}
	if !a.Username.Empty() {
		username, err := a.Username.Get()
		if err != nil {
			return fmt.Errorf("getting username failed: %w", err)
		}
		cfg.Username = username.String()
		username.Destroy()
	}
	if !a.Password.Empty() {
		password, err := a.Password.Get()
		if err != nil {
			return fmt.Errorf("getting password failed: %w", err)
		}
		cfg.Password = password.String()
		password.Destroy()
	}

	client, err := elasticsearch.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("creating client: %w", err)
	}

	// Fail early on wrong URL or credentials. The client also verifies that
	// the server is Elasticsearch.
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.Timeout))
	defer cancel()
	res, err := client.Info(client.Info.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("connecting to Elasticsearch: %w", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		return fmt.Errorf("connecting to Elasticsearch: %s", res.String())
	}

	a.client = client
	return nil
}

func (a *RempElasticsearch) Close() error {
	if a.client == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.Timeout))
	defer cancel()
	err := a.client.Close(ctx)
	a.client = nil
	return err
}

// Write sends the metrics in one bulk request. Metrics that can't be written
// because of their content (see badDocument) are rejected and dropped.
// Everything else that failed stays in the buffer and is retried on the next
// flush.
func (a *RempElasticsearch) Write(metrics []telegraf.Metric) error {
	var (
		body       bytes.Buffer
		sent       []int // bulk item index -> metric index
		accepted   []int
		rejected   []int
		rejectErrs []error
		retry      int
		reqErr     error
	)
	reject := func(i int, err error) {
		if len(rejected) < maxLoggedRejects {
			a.Log.Errorf("Dropping metric: %v", err)
		}
		rejected = append(rejected, i)
		rejectErrs = append(rejectErrs, err)
	}

	for i, m := range metrics {
		meta, doc, err := a.bulkItem(m)
		switch {
		case err != nil:
			reject(i, err)
		case meta == nil:
			accepted = append(accepted, i)
		default:
			body.Write(meta)
			body.WriteByte('\n')
			body.Write(doc)
			body.WriteByte('\n')
			sent = append(sent, i)
		}
	}

	if len(sent) > 0 {
		items, err := a.bulk(&body)
		if err == nil && len(items) != len(sent) {
			err = fmt.Errorf("bulk response has %d items, expected %d", len(items), len(sent))
		}
		if err != nil {
			// Nothing is known to be written; keep all sent metrics.
			reqErr = err
			retry = len(sent)
		}
		for j, item := range items {
			if reqErr != nil {
				break
			}
			var res bulkItemResp
			for _, r := range item {
				res = r
			}
			i := sent[j]
			switch {
			case res.Status >= 200 && res.Status < 300:
				accepted = append(accepted, i)
			case badDocument(res):
				reject(i, fmt.Errorf("elasticsearch rejected document %q in index %q: %s",
					res.ID, a.indexName(metrics[i]), res.Error))
			default:
				retry++
				if retry == 1 {
					a.Log.Warnf("Elasticsearch failed to write document %q (status %d): %s; will retry", res.ID, res.Status, res.Error)
				}
			}
		}
	}

	if len(rejected) == 0 && retry == 0 {
		return nil
	}
	// A failure that leaves every metric in the buffer is a plain error.
	if len(rejected) == 0 && len(accepted) == 0 && reqErr != nil {
		return reqErr
	}

	var msg []string
	if reqErr != nil {
		msg = append(msg, reqErr.Error())
	}
	if len(rejected) > 0 {
		msg = append(msg, fmt.Sprintf("%d of %d metrics dropped", len(rejected), len(metrics)))
	}
	if retry > 0 {
		msg = append(msg, fmt.Sprintf("%d of %d metrics will be retried", retry, len(metrics)))
	}
	return &internal.PartialWriteError{
		Err:                 errors.New(strings.Join(msg, "; ")),
		MetricsAccept:       accepted,
		MetricsReject:       rejected,
		MetricsRejectErrors: rejectErrs,
	}
}

// badDocument tells whether a bulk item failed because of the document itself
// (status 400: mapping conflict, script error, ...) and would fail the same way
// on every retry. A few 400 errors are caused by the target index instead; they
// are retried so that the data waits (in Kafka) until the index is fixed.
func badDocument(res bulkItemResp) bool {
	if res.Status != http.StatusBadRequest {
		return false
	}
	if res.Error == nil {
		return true
	}
	switch res.Error.Type {
	case "validation_exception", // e.g. shard limit reached when creating a new index
		"invalid_index_name_exception":
		return false
	case "illegal_argument_exception":
		// "no write index is defined for alias [...]"
		return !strings.Contains(res.Error.Reason, "write index")
	}
	return true
}

func (a *RempElasticsearch) bulk(body *bytes.Buffer) ([]map[string]bulkItemResp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.Timeout))
	defer cancel()

	res, err := a.client.Bulk(body,
		a.client.Bulk.WithContext(ctx),
		a.client.Bulk.WithFilterPath("items.*._id", "items.*.status", "items.*.error"),
	)
	if err != nil {
		return nil, fmt.Errorf("sending bulk request: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		return nil, fmt.Errorf("bulk request failed: %s", res.String())
	}

	var parsed struct {
		Items []map[string]bulkItemResp `json:"items"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decoding bulk response: %w", err)
	}
	return parsed.Items, nil
}

// bulkItem returns the action and source lines for the metric. A nil action
// with nil error means there is nothing to write.
func (a *RempElasticsearch) bulkItem(m telegraf.Metric) (meta, doc []byte, err error) {
	document, err := a.document(m)
	if err != nil {
		return nil, nil, err
	}

	var id string
	if a.IDField != "" {
		v, ok := document[a.IDField].(string)
		if !ok {
			return nil, nil, fmt.Errorf("metric %q: id field %q is missing or not a string (%T)", m.Name(), a.IDField, document[a.IDField])
		}
		id = v
	}

	var act action
	var source any
	if a.scriptSource == "" {
		act.Index = &actionMeta{Index: a.indexName(m), ID: id}
		source = document
	} else {
		fields := make(map[string]any, len(a.scriptFields))
		for _, f := range a.scriptFields {
			v, ok := document[f]
			if !ok && a.scriptSource == addScript {
				continue
			}
			// Missing updated_fields are written as null, same as in the
			// olivere/elastic based plugin.
			fields[f] = v
		}
		if len(fields) == 0 {
			a.Log.Debugf("Metric %q with id %q has none of added_fields %v, skipping", m.Name(), id, a.scriptFields)
			return nil, nil, nil
		}
		act.Update = &actionMeta{Index: a.indexName(m), ID: id, RetryOnConflict: retryOnConflict}
		source = updateBody{
			Script: script{Source: a.scriptSource, Lang: "painless", Params: map[string]any{"fields": fields}},
			Upsert: document,
		}
	}

	if meta, err = json.Marshal(act); err != nil {
		return nil, nil, fmt.Errorf("metric %q: encoding action: %w", m.Name(), err)
	}
	if doc, err = json.Marshal(source); err != nil {
		return nil, nil, fmt.Errorf("metric %q with id %q: encoding document: %w", m.Name(), id, err)
	}
	return meta, doc, nil
}

// document converts the metric to the Elasticsearch document: tags and fields
// on the top level, "_json" field merged in and the timestamp in "time".
func (a *RempElasticsearch) document(m telegraf.Metric) (map[string]any, error) {
	doc := make(map[string]any, len(m.TagList())+len(m.FieldList())+1)
	doc["time"] = m.Time().Format(time.RFC3339)

	for _, tag := range m.TagList() {
		// Before _json existed, the tracker sent booleans as "1"/"0" tags.
		switch tag.Value {
		case "":
			continue
		case "1":
			doc[tag.Key] = true
		case "0":
			doc[tag.Key] = false
		default:
			doc[tag.Key] = tag.Value
		}
	}

	for _, field := range m.FieldList() {
		if field.Key != jsonField {
			doc[field.Key] = field.Value
			continue
		}
		s, ok := field.Value.(string)
		if !ok {
			return nil, fmt.Errorf("metric %q: field %q has invalid type %T", m.Name(), jsonField, field.Value)
		}
		if err := json.Unmarshal([]byte(s), &doc); err != nil {
			return nil, fmt.Errorf("metric %q: decoding field %q: %w", m.Name(), jsonField, err)
		}
	}

	if a.whitelist != nil {
		for k := range doc {
			if !a.whitelist[k] {
				delete(doc, k)
			}
		}
	}
	return doc, nil
}

func (a *RempElasticsearch) indexName(m telegraf.Metric) string {
	name := a.IndexName
	if a.hasDates {
		t := m.Time().UTC()
		_, week := t.ISOWeek()
		name = strings.NewReplacer(
			"%Y", t.Format("2006"),
			"%y", t.Format("06"),
			"%m", t.Format("01"),
			"%d", t.Format("02"),
			"%H", t.Format("15"),
			// Not zero-padded, kept as in the olivere/elastic based plugin.
			"%V", strconv.Itoa(week),
		).Replace(name)
	}
	for _, p := range a.tagPatterns {
		v, ok := m.GetTag(p.key)
		if !ok {
			v = a.DefaultTagValue
		}
		name = strings.ReplaceAll(name, p.placeholder, v)
	}
	return name
}

func parseTagPatterns(indexName string) []tagPattern {
	var patterns []tagPattern
	rest := indexName
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			return patterns
		}
		end := strings.Index(rest[start:], "}}")
		if end < 0 {
			return patterns
		}
		placeholder := rest[start : start+end+2]
		patterns = append(patterns, tagPattern{
			placeholder: placeholder,
			key:         strings.TrimSpace(placeholder[2 : len(placeholder)-2]),
		})
		rest = rest[start+end+2:]
	}
}

func init() {
	outputs.Add("remp_elasticsearch", func() telegraf.Output {
		return &RempElasticsearch{
			Timeout: config.Duration(5 * time.Second),
		}
	})
}
