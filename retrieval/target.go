// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package retrieval

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/extraction"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/log"

	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/httputil"
)

const (
	// ScrapeHealthMetricName is the metric name for the synthetic health
	// variable.
	scrapeHealthMetricName model.LabelValue = "up"
	// ScrapeTimeMetricName is the metric name for the synthetic scrape duration
	// variable.
	scrapeDurationMetricName model.LabelValue = "scrape_duration_seconds"
	// Capacity of the channel to buffer samples during ingestion.
	ingestedSamplesCap = 256

	// Constants for instrumentation.
	namespace = "prometheus"
	interval  = "interval"
)

var (
	errIngestChannelFull = errors.New("ingestion channel full")

	targetIntervalLength = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace:  namespace,
			Name:       "target_interval_length_seconds",
			Help:       "Actual intervals between scrapes.",
			Objectives: map[float64]float64{0.01: 0.001, 0.05: 0.005, 0.5: 0.05, 0.90: 0.01, 0.99: 0.001},
		},
		[]string{interval},
	)
)

func init() {
	prometheus.MustRegister(targetIntervalLength)
}

// TargetHealth describes the health state of a target.
type TargetHealth int

func (t TargetHealth) String() string {
	switch t {
	case HealthUnknown:
		return "unknown"
	case HealthGood:
		return "healthy"
	case HealthBad:
		return "unhealthy"
	}
	panic("unknown state")
}

const (
	// Unknown is the state of a Target before it is first scraped.
	HealthUnknown TargetHealth = iota
	// Healthy is the state of a Target that has been successfully scraped.
	HealthGood
	// Unhealthy is the state of a Target that was scraped unsuccessfully.
	HealthBad
)

// TargetStatus contains information about the current status of a scrape target.
type TargetStatus struct {
	lastError  error
	lastScrape time.Time
	health     TargetHealth

	mu sync.RWMutex
}

// LastError returns the error encountered during the last scrape.
func (ts *TargetStatus) LastError() error {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.lastError
}

// LastScrape returns the time of the last scrape.
func (ts *TargetStatus) LastScrape() time.Time {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.lastScrape
}

// Health returns the last known health state of the target.
func (ts *TargetStatus) Health() TargetHealth {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.health
}

func (ts *TargetStatus) setLastScrape(t time.Time) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.lastScrape = t
}

func (ts *TargetStatus) setLastError(err error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if err == nil {
		ts.health = HealthGood
	} else {
		ts.health = HealthBad
	}
	ts.lastError = err
}

// Target refers to a singular HTTP or HTTPS endpoint.
type Target struct {
	// The status object for the target. It is only set once on initialization.
	status *TargetStatus
	// Closing scraperStopping signals that scraping should stop.
	scraperStopping chan struct{}
	// Closing scraperStopped signals that scraping has been stopped.
	scraperStopped chan struct{}
	// Channel to buffer ingested samples.
	ingestedSamples chan model.Samples

	// Mutex protects the members below.
	sync.RWMutex
	// The HTTP client used to scrape the target's endpoint.
	httpClient *http.Client
	// url is the URL to be scraped. Its host is immutable.
	url *url.URL
	// Labels before any processing.
	metaLabels model.LabelSet
	// Any base labels that are added to this target and its metrics.
	baseLabels model.LabelSet
	// What is the deadline for the HTTP or HTTPS against this endpoint.
	deadline time.Duration
	// The time between two scrapes.
	scrapeInterval time.Duration
	// Whether the target's labels have precedence over the base labels
	// assigned by the scraping instance.
	honorLabels bool
	// Metric relabel configuration.
	metricRelabelConfigs []*config.RelabelConfig
}

// NewTarget creates a reasonably configured target for querying.
func NewTarget(cfg *config.ScrapeConfig, baseLabels, metaLabels model.LabelSet) *Target {
	t := &Target{
		url: &url.URL{
			Scheme: string(baseLabels[model.SchemeLabel]),
			Host:   string(baseLabels[model.AddressLabel]),
		},
		status:          &TargetStatus{},
		scraperStopping: make(chan struct{}),
		scraperStopped:  make(chan struct{}),
	}
	t.Update(cfg, baseLabels, metaLabels)
	return t
}

// Status returns the status of the target.
func (t *Target) Status() *TargetStatus {
	return t.status
}

// Update overwrites settings in the target that are derived from the job config
// it belongs to.
func (t *Target) Update(cfg *config.ScrapeConfig, baseLabels, metaLabels model.LabelSet) {
	t.Lock()
	defer t.Unlock()

	httpClient, err := newHTTPClient(cfg)
	if err != nil {
		log.Errorf("cannot create HTTP client: %v", err)
		return
	}
	t.httpClient = httpClient

	t.url.Scheme = string(baseLabels[model.SchemeLabel])
	t.url.Path = string(baseLabels[model.MetricsPathLabel])
	params := url.Values{}
	for k, v := range cfg.Params {
		params[k] = make([]string, len(v))
		copy(params[k], v)
	}
	for k, v := range baseLabels {
		if strings.HasPrefix(string(k), model.ParamLabelPrefix) {
			if len(params[string(k[len(model.ParamLabelPrefix):])]) > 0 {
				params[string(k[len(model.ParamLabelPrefix):])][0] = string(v)
			} else {
				params[string(k[len(model.ParamLabelPrefix):])] = []string{string(v)}
			}
		}
	}
	t.url.RawQuery = params.Encode()

	t.scrapeInterval = time.Duration(cfg.ScrapeInterval)
	t.deadline = time.Duration(cfg.ScrapeTimeout)

	t.honorLabels = cfg.HonorLabels
	t.metaLabels = metaLabels
	t.baseLabels = model.LabelSet{}
	// All remaining internal labels will not be part of the label set.
	for name, val := range baseLabels {
		if !strings.HasPrefix(string(name), model.ReservedLabelPrefix) {
			t.baseLabels[name] = val
		}
	}
	if _, ok := t.baseLabels[model.InstanceLabel]; !ok {
		t.baseLabels[model.InstanceLabel] = model.LabelValue(t.InstanceIdentifier())
	}
	t.metricRelabelConfigs = cfg.MetricRelabelConfigs
}

func newHTTPClient(cfg *config.ScrapeConfig) (*http.Client, error) {
	tlsConfig := &tls.Config{}

	// If a CA cert is provided then let's read it in so we can validate the
	// scrape target's certificate properly.
	if len(cfg.CACert) > 0 {
		caCertPool := x509.NewCertPool()
		// Load CA cert.
		caCert, err := ioutil.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("Unable to use specified CA cert %s: %s", cfg.CACert, err)
		}
		caCertPool.AppendCertsFromPEM(caCert)
		tlsConfig.RootCAs = caCertPool
	}

	// If a client cert & key is provided then configure TLS config accordingly.
	if cfg.ClientCert != nil && len(cfg.ClientCert.Cert) > 0 && len(cfg.ClientCert.Key) > 0 {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCert.Cert, cfg.ClientCert.Key)
		if err != nil {
			return nil, fmt.Errorf("Unable to use specified client cert (%s) & key (%s): %s", cfg.ClientCert.Cert, cfg.ClientCert.Key, err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	tlsConfig.BuildNameToCertificate()

	// Get a default roundtripper with the scrape timeout.
	rt := httputil.NewDeadlineRoundTripper(time.Duration(cfg.ScrapeTimeout), cfg.ProxyURL.URL)
	tr := rt.(*http.Transport)
	// Set the TLS config from above
	tr.TLSClientConfig = tlsConfig
	rt = tr

	// If a bearer token is provided, create a round tripper that will set the
	// Authorization header correctly on each request.
	bearerToken := cfg.BearerToken
	if len(bearerToken) == 0 && len(cfg.BearerTokenFile) > 0 {
		if b, err := ioutil.ReadFile(cfg.BearerTokenFile); err != nil {
			return nil, fmt.Errorf("Unable to read bearer token file %s: %s", cfg.BearerTokenFile, err)
		} else {
			bearerToken = string(b)
		}
	}
	if len(bearerToken) > 0 {
		rt = httputil.NewBearerAuthRoundTripper(bearerToken, rt)
	}

	if cfg.BasicAuth != nil {
		rt = httputil.NewBasicAuthRoundTripper(cfg.BasicAuth.Username, cfg.BasicAuth.Password, rt)
	}

	// Return a new client with the configured round tripper.
	return httputil.NewClient(rt), nil
}

func (t *Target) String() string {
	return t.url.Host
}

// Ingest implements an extraction.Ingester.
func (t *Target) Ingest(s model.Samples) error {
	t.RLock()
	deadline := t.deadline
	t.RUnlock()
	// Since the regular case is that ingestedSamples is ready to receive,
	// first try without setting a timeout so that we don't need to allocate
	// a timer most of the time.
	select {
	case t.ingestedSamples <- s:
		return nil
	default:
		select {
		case t.ingestedSamples <- s:
			return nil
		case <-time.After(deadline / 10):
			return errIngestChannelFull
		}
	}
}

// Ensure that Target implements extraction.Ingester at compile time.
var _ extraction.Ingester = (*Target)(nil)

// RunScraper implements Target.
func (t *Target) RunScraper(sampleAppender storage.SampleAppender) {
	defer close(t.scraperStopped)

	t.RLock()
	lastScrapeInterval := t.scrapeInterval
	t.RUnlock()

	log.Debugf("Starting scraper for target %v...", t)

	jitterTimer := time.NewTimer(time.Duration(float64(lastScrapeInterval) * rand.Float64()))
	select {
	case <-jitterTimer.C:
	case <-t.scraperStopping:
		jitterTimer.Stop()
		return
	}
	jitterTimer.Stop()

	ticker := time.NewTicker(lastScrapeInterval)
	defer ticker.Stop()

	t.status.setLastScrape(time.Now())
	t.scrape(sampleAppender)

	// Explanation of the contraption below:
	//
	// In case t.scraperStopping has something to receive, we want to read
	// from that channel rather than starting a new scrape (which might take very
	// long). That's why the outer select has no ticker.C. Should t.scraperStopping
	// not have anything to receive, we go into the inner select, where ticker.C
	// is in the mix.
	for {
		select {
		case <-t.scraperStopping:
			return
		default:
			select {
			case <-t.scraperStopping:
				return
			case <-ticker.C:
				took := time.Since(t.status.LastScrape())
				t.status.setLastScrape(time.Now())

				intervalStr := lastScrapeInterval.String()

				t.RLock()
				// On changed scrape interval the new interval becomes effective
				// after the next scrape.
				if lastScrapeInterval != t.scrapeInterval {
					ticker.Stop()
					ticker = time.NewTicker(t.scrapeInterval)
					lastScrapeInterval = t.scrapeInterval
				}
				t.RUnlock()

				targetIntervalLength.WithLabelValues(intervalStr).Observe(
					float64(took) / float64(time.Second), // Sub-second precision.
				)
				t.scrape(sampleAppender)
			}
		}
	}
}

// StopScraper implements Target.
func (t *Target) StopScraper() {
	log.Debugf("Stopping scraper for target %v...", t)

	close(t.scraperStopping)
	<-t.scraperStopped

	log.Debugf("Scraper for target %v stopped.", t)
}

const acceptHeader = `application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=delimited;q=0.7,text/plain;version=0.0.4;q=0.3,application/json;schema="prometheus/telemetry";version=0.0.2;q=0.2,*/*;q=0.1`

func (t *Target) scrape(sampleAppender storage.SampleAppender) (err error) {
	start := time.Now()
	baseLabels := t.BaseLabels()

	t.RLock()
	var (
		honorLabels          = t.honorLabels
		httpClient           = t.httpClient
		metricRelabelConfigs = t.metricRelabelConfigs
	)
	t.RUnlock()

	defer func() {
		t.status.setLastError(err)
		recordScrapeHealth(sampleAppender, model.TimeFromTime(start), baseLabels, t.status.Health(), time.Since(start))
	}()

	req, err := http.NewRequest("GET", t.URL().String(), nil)
	if err != nil {
		panic(err)
	}
	req.Header.Add("Accept", acceptHeader)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned HTTP status %s", resp.Status)
	}

	processor, err := extraction.ProcessorForRequestHeader(resp.Header)
	if err != nil {
		return err
	}

	t.ingestedSamples = make(chan model.Samples, ingestedSamplesCap)

	processOptions := &extraction.ProcessOptions{
		Timestamp: model.TimeFromTime(start),
	}
	go func() {
		err = processor.ProcessSingle(resp.Body, t, processOptions)
		close(t.ingestedSamples)
	}()

	for samples := range t.ingestedSamples {
		for _, s := range samples {
			if honorLabels {
				// Merge the metric with the baseLabels for labels not already set in the
				// metric. This also considers labels explicitly set to the empty string.
				for ln, lv := range baseLabels {
					if _, ok := s.Metric[ln]; !ok {
						s.Metric[ln] = lv
					}
				}
			} else {
				// Merge the ingested metric with the base label set. On a collision the
				// value of the label is stored in a label prefixed with the exported prefix.
				for ln, lv := range baseLabels {
					if v, ok := s.Metric[ln]; ok && v != "" {
						s.Metric[model.ExportedLabelPrefix+ln] = v
					}
					s.Metric[ln] = lv
				}
			}
			// Avoid the copy in Relabel if there are no configs.
			if len(metricRelabelConfigs) > 0 {
				labels, err := Relabel(model.LabelSet(s.Metric), metricRelabelConfigs...)
				if err != nil {
					log.Errorf("Error while relabeling metric %s of instance %s: %s", s.Metric, req.URL, err)
					continue
				}
				// Check if the timeseries was dropped.
				if labels == nil {
					continue
				}
				s.Metric = model.Metric(labels)
			}
			sampleAppender.Append(s)
		}
	}
	return err
}

// URL returns a copy of the target's URL.
func (t *Target) URL() *url.URL {
	t.RLock()
	defer t.RUnlock()
	u := &url.URL{}
	*u = *t.url
	return u
}

// InstanceIdentifier returns the identifier for the target.
func (t *Target) InstanceIdentifier() string {
	return t.url.Host
}

// fullLabels returns the base labels plus internal labels defining the target.
func (t *Target) fullLabels() model.LabelSet {
	t.RLock()
	defer t.RUnlock()
	lset := make(model.LabelSet, len(t.baseLabels)+2)
	for ln, lv := range t.baseLabels {
		lset[ln] = lv
	}
	lset[model.MetricsPathLabel] = model.LabelValue(t.url.Path)
	lset[model.AddressLabel] = model.LabelValue(t.url.Host)
	lset[model.SchemeLabel] = model.LabelValue(t.url.Scheme)
	return lset
}

// BaseLabels returns a copy of the target's base labels.
func (t *Target) BaseLabels() model.LabelSet {
	t.RLock()
	defer t.RUnlock()
	lset := make(model.LabelSet, len(t.baseLabels))
	for ln, lv := range t.baseLabels {
		lset[ln] = lv
	}
	return lset
}

// MetaLabels returns a copy of the target's labels before any processing.
func (t *Target) MetaLabels() model.LabelSet {
	t.RLock()
	defer t.RUnlock()
	lset := make(model.LabelSet, len(t.metaLabels))
	for ln, lv := range t.metaLabels {
		lset[ln] = lv
	}
	return lset
}

func recordScrapeHealth(
	sampleAppender storage.SampleAppender,
	timestamp model.Time,
	baseLabels model.LabelSet,
	health TargetHealth,
	scrapeDuration time.Duration,
) {
	healthMetric := make(model.Metric, len(baseLabels)+1)
	durationMetric := make(model.Metric, len(baseLabels)+1)

	healthMetric[model.MetricNameLabel] = model.LabelValue(scrapeHealthMetricName)
	durationMetric[model.MetricNameLabel] = model.LabelValue(scrapeDurationMetricName)

	for label, value := range baseLabels {
		healthMetric[label] = value
		durationMetric[label] = value
	}

	healthValue := model.SampleValue(0)
	if health == HealthGood {
		healthValue = model.SampleValue(1)
	}

	healthSample := &model.Sample{
		Metric:    healthMetric,
		Timestamp: timestamp,
		Value:     healthValue,
	}
	durationSample := &model.Sample{
		Metric:    durationMetric,
		Timestamp: timestamp,
		Value:     model.SampleValue(float64(scrapeDuration) / float64(time.Second)),
	}

	sampleAppender.Append(healthSample)
	sampleAppender.Append(durationSample)
}
