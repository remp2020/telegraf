package elasticsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/plugins/common/tls"
	"github.com/influxdata/telegraf/plugins/outputs"
	"github.com/olivere/elastic"
)

// Elasticsearch represents elasticsearch-metric.
type Elasticsearch struct {
	FieldWhitelist      []string `toml:"field_whitelist"`
	IndexName           string
	TypeName            string   `toml:"type_name"`
	IDField             string   `toml:"id_field"`
	UpdateQueryField    string   `toml:"update_query_field"`
	UpdatedFields       []string `toml:"updated_fields"`
	IncrementedFields   []string `toml:"incremented_fields"`
	DefaultTagValue     string
	TagKeys             []string
	Username            string
	Password            string
	Log                 telegraf.Logger `toml:"-"`
	EnableSniffer       bool
	Timeout             config.Duration
	HealthCheckInterval config.Duration
	ManageTemplate      bool
	TemplateName        string
	OverwriteTemplate   bool
	SSLCA               string   `toml:"ssl_ca"`   // Path to CA file
	SSLCert             string   `toml:"ssl_cert"` // Path to host cert file
	SSLKey              string   `toml:"ssl_key"`  // Path to cert key file
	URLs                []string `toml:"urls"`
	InsecureSkipVerify  bool     // Use SSL but skip chain & host verification
	tls.ClientConfig

	Client *elastic.Client
}

var sampleConfig = `
  ## The full HTTP endpoint URL for your Elasticsearch instance
  ## Multiple urls can be specified as part of the same cluster,
  ## this means that only ONE of the urls will be written to each interval.
  urls = [ "http://node1.es.example.com:9200" ] # required.
  ## Elasticsearch client timeout, defaults to "5s" if not set.
  timeout = "5s"
  ## Set to true to ask Elasticsearch a list of all cluster nodes,
  ## thus it is not necessary to list all nodes in the urls config option.
  enable_sniffer = false
  ## Set the interval to check if the Elasticsearch nodes are available
  ## Setting to "0s" will disable the health check (not recommended in production)
  health_check_interval = "10s"
  ## HTTP basic authentication details (eg. when using Shield)
  # username = "telegraf"
  # password = "mypassword"

  ## Index Config
  ## The target index for metrics (Elasticsearch will create if it not exists).
  ## You can use the date specifiers below to create indexes per time frame.
  ## The metric timestamp will be used to decide the destination index name
  # %Y - year (2016)
  # %y - last two digits of year (00..99)
  # %m - month (01..12)
  # %d - day of month (e.g., 01)
  # %H - hour (00..23)
  # %V - week of the year (ISO week) (01..53)
  ## Additionally, you can specify a tag name using the notation {{tag_name}}
  ## which will be used as part of the index name. If the tag does not exist,
  ## the default tag value will be used.
  # index_name = "telegraf-{{host}}-%Y.%m.%d"
  # default_tag_value = "none"
  index_name = "telegraf-%Y.%m.%d" # required.

  ## Optional TLS Config
  # tls_ca = "/etc/telegraf/ca.pem"
  # tls_cert = "/etc/telegraf/cert.pem"
  # tls_key = "/etc/telegraf/key.pem"
  ## Use TLS but skip chain & host verification
  # insecure_skip_verify = false

  ## Template Config
  ## Set to true if you want telegraf to manage its index template.
  ## If enabled it will create a recommended index template for telegraf indexes
  manage_template = true
  ## The template name used for telegraf indexes
  template_name = "telegraf"
  ## Set to true if you want telegraf to overwrite an existing template
  overwrite_template = false

  ## REMP configs
  ## Custom ID field if you want to manage index IDs yourself
  # id_field = "remp_pageview_id
  ## Name of the index to be used, defaults to name of tracked metric
  # index_name = "pageviews_time_spent"
  ## Name of the type to be used within index, defaults to _doc
  # type_name = "_doc"
  ## List of fields to be updated (implicitly triggers update call instead index)
  # updated_fields = ["timespent"]
  ## List of fields to be incremented (implicitly triggers update call instead index)
  # incremented_fields = ["clicks"]
  ## List of fields to be included in index - mimics taginclude which doesn't work
  ## in remp_elastic as REMP tracks JSON-encoded string to preserve types.
  ## These fields are protected and don't need to be whitelisted:
  ## ["category", "action", "browser_id", "user_id", "article_id", "remp_pageview_id", "remp_session_id", "url", "time"]
  # field_whitelist = ["subscriber", "foo"]
`

// Connect connects and authenticates to Elasticsearch instance.
func (a *Elasticsearch) Connect() error {
	if a.URLs == nil || a.IndexName == "" {
		return fmt.Errorf("Elasticsearch urls or index_name is not defined")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.Timeout))
	defer cancel()

	var clientOptions []elastic.ClientOptionFunc

	// Set tls config
	tlsCfg, err := a.ClientConfig.TLSConfig()
	if err != nil {
		return err
	}
	tr := &http.Transport{
		TLSClientConfig: tlsCfg,
	}

	httpclient := &http.Client{
		Transport: tr,
		Timeout:   time.Duration(a.Timeout),
	}

	clientOptions = append(clientOptions,
		elastic.SetHttpClient(httpclient),
		elastic.SetSniff(a.EnableSniffer),
		elastic.SetURL(a.URLs...),
		elastic.SetHealthcheckInterval(time.Duration(a.HealthCheckInterval)),
	)

	if a.Username != "" && a.Password != "" {
		clientOptions = append(clientOptions,
			elastic.SetBasicAuth(a.Username, a.Password),
		)
	}

	if time.Duration(a.HealthCheckInterval) == 0 {
		clientOptions = append(clientOptions,
			elastic.SetHealthcheck(false),
		)
		a.Log.Debugf("Disabling health check")
	}

	client, err := elastic.NewClient(clientOptions...)

	if err != nil {
		return err
	}

	// check for ES version on first node
	esVersion, err := client.ElasticsearchVersion(a.URLs[0])

	if err != nil {
		return fmt.Errorf("Elasticsearch version check failed: %s", err)
	}

	// quit if ES version is not supported
	i, err := strconv.Atoi(strings.Split(esVersion, ".")[0])
	if err != nil || i < 5 {
		return fmt.Errorf("Elasticsearch version not supported: %s", esVersion)
	}

	a.Log.Infof("Elasticsearch version: %q", esVersion)

	a.Client = client

	if a.ManageTemplate {
		err := a.manageTemplate(ctx)
		if err != nil {
			return err
		}
	}

	a.IndexName, a.TagKeys = a.GetTagKeys(a.IndexName)

	return nil
}

// Write processes and writes metrics to Elasticsearch.
func (a *Elasticsearch) Write(metrics []telegraf.Metric) error {
	if len(metrics) == 0 {
		return nil
	}

	bulkRequest := a.Client.Bulk()

	for _, metric := range metrics {
		var indexBase string
		if a.IndexName != "" {
			indexBase = a.IndexName
		} else {
			indexBase = metric.Name()
		}

		// index name has to be re-evaluated each time for telegraf
		// to send the metric to the correct time-based index
		indexName := a.GetIndexName(indexBase, metric.Time(), a.TagKeys, metric.Tags())

		var typeName string
		if a.TypeName != "" {
			typeName = a.TypeName
		} else {
			typeName = "_doc"
		}

		m := make(map[string]interface{})

		m["time"] = metric.Time().Format(time.RFC3339)

		// booleans are being pushed to queue as 0/1 values (so they could be used as string tags in InfluxDB)
		// now it's time to convert them back to what they are
		for field, val := range metric.Tags() {
			// legacy, so we still can parse bool values from pre _json era
			switch val {
			case "":
				continue
			case "1":
				m[field] = true
			case "0":
				m[field] = false
			default:
				m[field] = val
			}
		}
		for field, val := range metric.Fields() {
			// internal type; _json contains JSON encoded string to preserve types
			if field == "_json" {
				stringVal, ok := val.(string)
				if !ok {
					return fmt.Errorf("invalid type of [_json] field: %T", val)
				}
				err := json.Unmarshal([]byte(stringVal), &m)
				if err != nil {
					return err
				}
				continue
			}
			m[field] = val
		}

		m = a.filterMetric(m)

		handleBulkUpdate := func(a *Elasticsearch, operator string) (bool, error) {
			if a.IDField == "" {
				return false, fmt.Errorf("unable to update Elasticsearch records, no explicit ID field provided")
			}
			idValue, ok := m[a.IDField].(string)
			if !ok {
				a.Log.Errorf("Unable to use value of %s as ID, non-string value received: %T", a.IDField, m[a.IDField])
				a.Log.Infof("Metric content: %#v", m)
				return false, nil
			}

			var scriptSource string
			scriptParams := make(map[string]interface{})
			for _, field := range a.UpdatedFields {
				// prepare script to update existing value
				scriptSource = fmt.Sprintf("%s ctx._source.%s %s params.%s;", scriptSource, field, operator, field)
				scriptParams[field] = m[field]
			}

			updateRequest := elastic.
				NewBulkUpdateRequest().
				Index(indexName).
				Type(typeName).
				Id(idValue).
				Script(elastic.NewScript(scriptSource).Lang("painless").Params(scriptParams)).
				Upsert(m)

			bulkRequest.Add(updateRequest)
			return true, nil
		}

		if len(a.UpdatedFields) > 0 {
			ok, err := handleBulkUpdate(a, "=")
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
		} else if len(a.IncrementedFields) > 0 {
			ok, err := handleBulkUpdate(a, "+=")
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
		} else {
			indexRequest := elastic.NewBulkIndexRequest().
				Index(indexName).
				Type(typeName).
				Doc(m)

			if a.IDField != "" {
				idValue, ok := m[a.IDField].(string)
				if !ok {
					a.Log.Errorf("Unable to use value of %s as ID, non-string value received: %T", a.IDField, m[a.IDField])
					a.Log.Infof("Metric content: %#v", m)
					continue
				}
				indexRequest = indexRequest.Id(idValue)
			}

			bulkRequest.Add(indexRequest)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.Timeout))
	defer cancel()

	res, err := bulkRequest.Do(ctx)

	if err != nil {
		return fmt.Errorf("Error sending bulk request to Elasticsearch: %s", err)
	}

	if res.Errors {
		for id, err := range res.Failed() {
			a.Log.Errorf("Elasticsearch indexing failure, id: %d, error: %s, caused by: %s, %s", id, err.Error.Reason, err.Error.CausedBy["reason"], err.Error.CausedBy["type"])
			break
		}
		return fmt.Errorf("W! Elasticsearch failed to index %d metrics", len(res.Failed()))
	}

	return nil

}

func (a *Elasticsearch) manageTemplate(ctx context.Context) error {
	if a.TemplateName == "" {
		return fmt.Errorf("Elasticsearch template_name configuration not defined")
	}

	templateExists, errExists := a.Client.IndexTemplateExists(a.TemplateName).Do(ctx)

	if errExists != nil {
		return fmt.Errorf("Elasticsearch template check failed, template name: %s, error: %s", a.TemplateName, errExists)
	}

	templatePattern := a.IndexName

	if strings.Contains(templatePattern, "%") {
		templatePattern = templatePattern[0:strings.Index(templatePattern, "%")]
	}

	if strings.Contains(templatePattern, "{{") {
		templatePattern = templatePattern[0:strings.Index(templatePattern, "{{")]
	}

	if templatePattern == "" {
		return fmt.Errorf("Template cannot be created for dynamic index names without an index prefix")
	}

	if (a.OverwriteTemplate) || (!templateExists) || (templatePattern != "") {
		// Create or update the template
		tmpl := fmt.Sprintf(`
			{
				"template":"%s",
				"settings": {
					"index": {
						"refresh_interval": "10s",
						"mapping.total_fields.limit": 5000
					}
				},
				"mappings" : {
					"_default_" : {
						"_all": { "enabled": false	  },
						"properties" : {
							"@timestamp" : { "type" : "date" },
							"measurement_name" : { "type" : "keyword" }
						},
						"dynamic_templates": [
							{
								"tags": {
									"match_mapping_type": "string",
									"path_match": "tag.*",
									"mapping": {
										"ignore_above": 512,
										"type": "keyword"
									}
								}
							},
							{
								"metrics_long": {
									"match_mapping_type": "long",
									"mapping": {
										"type": "float",
										"index": false
									}
								}
							},
							{
								"metrics_double": {
									"match_mapping_type": "double",
									"mapping": {
										"type": "float",
										"index": false
									}
								}
							},
							{
								"text_fields": {
									"match": "*",
									"mapping": {
										"norms": false
									}
								}
							}
						]
					}
				}
			}`, templatePattern+"*")
		_, errCreateTemplate := a.Client.IndexPutTemplate(a.TemplateName).BodyString(tmpl).Do(ctx)

		if errCreateTemplate != nil {
			return fmt.Errorf("Elasticsearch failed to create index template %s : %s", a.TemplateName, errCreateTemplate)
		}

		a.Log.Debugf("Template %s created or updated\n", a.TemplateName)
	} else {
		a.Log.Debug("Found existing Elasticsearch template. Skipping template management")
	}
	return nil
}

//GetTagKeys returns list of tag keys.
func (a *Elasticsearch) GetTagKeys(indexName string) (string, []string) {

	tagKeys := []string{}
	startTag := strings.Index(indexName, "{{")

	for startTag >= 0 {
		endTag := strings.Index(indexName, "}}")

		if endTag < 0 {
			startTag = -1

		} else {
			tagName := indexName[startTag+2 : endTag]

			var tagReplacer = strings.NewReplacer(
				"{{"+tagName+"}}", "%s",
			)

			indexName = tagReplacer.Replace(indexName)
			tagKeys = append(tagKeys, (strings.TrimSpace(tagName)))

			startTag = strings.Index(indexName, "{{")
		}
	}

	return indexName, tagKeys
}

// GetIndexName returns name of the target index considering the date within provided index name.
func (a *Elasticsearch) GetIndexName(indexName string, eventTime time.Time, tagKeys []string, metricTags map[string]string) string {
	if strings.Contains(indexName, "%") {
		var dateReplacer = strings.NewReplacer(
			"%Y", eventTime.UTC().Format("2006"),
			"%y", eventTime.UTC().Format("06"),
			"%m", eventTime.UTC().Format("01"),
			"%d", eventTime.UTC().Format("02"),
			"%H", eventTime.UTC().Format("15"),
			"%V", getISOWeek(eventTime.UTC()),
		)

		indexName = dateReplacer.Replace(indexName)
	}

	tagValues := []interface{}{}

	for _, key := range tagKeys {
		if value, ok := metricTags[key]; ok {
			tagValues = append(tagValues, value)
		} else {
			a.Log.Debugf("Tag '%s' not found, using '%s' on index name instead\n", key, a.DefaultTagValue)
			tagValues = append(tagValues, a.DefaultTagValue)
		}
	}

	return fmt.Sprintf(indexName, tagValues...)

}

func getISOWeek(eventTime time.Time) string {
	_, week := eventTime.ISOWeek()
	return strconv.Itoa(week)
}

// SampleConfig returns sample configuration structure..
func (a *Elasticsearch) SampleConfig() string {
	return sampleConfig
}

// Description returns plugin text description.
func (a *Elasticsearch) Description() string {
	return "Configuration for Elasticsearch to send metrics to."
}

// Close unsets the client instance and makes it unusable.
func (a *Elasticsearch) Close() error {
	a.Client = nil
	return nil
}

// filterMetric removes tags according to FieldWhitelist definition.
func (a *Elasticsearch) filterMetric(metric map[string]interface{}) map[string]interface{} {
	protected := map[string]bool{
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

	include := make(map[string]bool)
	for _, tag := range a.FieldWhitelist {
		include[tag] = true
	}

	if len(a.FieldWhitelist) > 0 {
		for tag := range metric {
			if _, ok := protected[tag]; ok {
				continue
			}
			if _, ok := include[tag]; !ok {
				delete(metric, tag)
			}
		}
	}

	return metric
}

func init() {
	outputs.Add("remp_elasticsearch", func() telegraf.Output {
		return &Elasticsearch{
			Timeout:             config.Duration(time.Second * 5),
			HealthCheckInterval: config.Duration(time.Second * 10),
		}
	})
}
