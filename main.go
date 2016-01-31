package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "aurora"

var (
	addr       = flag.String("web.listen-address", ":9113", "Address to listen on for web interface and telemetry.")
	auroraURL  = flag.String("exporter.aurora-url", "http://127.0.0.1:8081", "URL to an Aurora scheduler or ZooKeeper ensemble")
	metricPath = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
)

var noLables = []string{}

var httpClient = http.Client{
	Transport: &http.Transport{
		MaxIdleConnsPerHost:   2,
		ResponseHeaderTimeout: 10 * time.Second,
		Dial: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 10 * time.Second,
		}).Dial,
	},
}

type exporter struct {
	sync.Mutex
	f            finder
	errors       prometheus.Counter
	duration     prometheus.Gauge
	pendingTasks *prometheus.GaugeVec
}

type pendingTask struct {
	PenaltyMs int      `json:"penaltyMs"`
	TaskIds   []string `json:"taskIds"`
	Name      string
}

func newAuroraExporter(f finder) *exporter {
	return &exporter{
		f: f,
		errors: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "exporter_scrape_errors_total",
				Help:      "Total scrape errors",
			}),
		duration: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "exporter_last_scrape_duration_seconds",
				Help:      "The last scrape duration",
			}),
		pendingTasks: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "tasks_pending",
				Help:      "Number of pending tasks, by job",
			},
			[]string{"role", "env", "job"},
		),
	}
}

func (e *exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.duration.Desc()
	ch <- e.errors.Desc()
}

func (e *exporter) Collect(ch chan<- prometheus.Metric) {
	e.Lock()
	defer e.Unlock()

	metricsChan := make(chan prometheus.Metric)
	go e.scrape(metricsChan)

	for metric := range metricsChan {
		ch <- metric
	}

	ch <- e.errors
	ch <- e.duration
}

func (e *exporter) parsePending(url string, ch chan<- prometheus.Metric) error {
	pendingResp, err := httpClient.Get(fmt.Sprintf("%s/pendingtasks", url))
	if err != nil {
		return err
	}
	defer pendingResp.Body.Close()

	pending := make([]pendingTask, 0)
	if err = json.NewDecoder(pendingResp.Body).Decode(&pending); err != nil {
		return err
	}

	for _, task := range pending {
		jobKey := strings.Split(task.Name, "/")
		count := len(task.TaskIds)
		metric := e.pendingTasks.WithLabelValues(jobKey[0], jobKey[1], jobKey[2])
		metric.Set(float64(count))
		ch <- metric
	}

	return nil
}

func (e *exporter) parseVars(url string, ch chan<- prometheus.Metric) error {
	resp, err := httpClient.Get(fmt.Sprintf("%s/vars.json", url))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var vars map[string]interface{}
	if err = json.NewDecoder(resp.Body).Decode(&vars); err != nil {
		return err
	}

	for name, v := range vars {
		v, ok := v.(float64)
		if !ok {
			continue
		}

		if desc, ok := counters[name]; ok {
			ch <- prometheus.MustNewConstMetric(
				desc,
				prometheus.CounterValue,
				v, noLables...,
			)
		}

		if desc, ok := gauges[name]; ok {
			ch <- prometheus.MustNewConstMetric(
				desc,
				prometheus.GaugeValue,
				v, noLables...,
			)
		}

		labelVars(ch, name, v)
	}

	return nil
}

func (e *exporter) scrape(ch chan<- prometheus.Metric) {
	defer close(ch)

	now := time.Now().UnixNano()
	defer func() {
		e.duration.Set(float64(time.Now().UnixNano()-now) / 1000000000)
	}()

	recordErr := func(err error) {
		glog.Warning(err)
		e.errors.Inc()
	}

	url, err := e.f.leaderURL()
	if err != nil {
		recordErr(err)
		return
	}

	if err = e.parsePending(url, ch); err != nil {
		recordErr(err)
	}

	if err = e.parseVars(url, ch); err != nil {
		recordErr(err)
	}
}

func main() {
	flag.Parse()

	finder, err := newFinder(*auroraURL)
	if err != nil {
		log.Fatal(err)
	}

	exporter := newAuroraExporter(finder)
	prometheus.MustRegister(exporter)

	http.Handle(*metricPath, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, *metricPath, http.StatusMovedPermanently)
	})

	glog.Info("starting aurora_exporter on ", *addr)

	log.Fatal(http.ListenAndServe(*addr, nil))
}
