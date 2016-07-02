package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/log"
)

const (
	namespace = "apache" // For Prometheus metrics.
)

var (
	listeningAddress = flag.String("telemetry.address", ":9117", "Address on which to expose metrics.")
	metricsEndpoint  = flag.String("telemetry.endpoint", "/metrics", "Path under which to expose metrics.")
	scrapeURI        = flag.String("scrape_uri", "http://localhost/server-status/?auto", "URI to apache stub status page.")
	insecure         = flag.Bool("insecure", false, "Ignore server certificate if using https.")
)

type Exporter struct {
	URI    string
	mutex  sync.RWMutex
	client *http.Client

	scrapeFailures prometheus.Counter
	accessesTotal  prometheus.Counter
	kBytesTotal    prometheus.Counter
	uptime         prometheus.Counter
	threads        *prometheus.GaugeVec
	workers        *prometheus.GaugeVec
}

func NewExporter(uri string) *Exporter {
	return &Exporter{
		URI: uri,
		scrapeFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_scrape_failures_total",
			Help:      "Number of errors while scraping apache.",
		}),
		accessesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "accesses_total",
			Help:      "Current total apache accesses",
		}),
		kBytesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "sent_kilobytes_total",
			Help:      "Current total kbytes sent",
		}),
		uptime: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "uptime_seconds_total",
			Help:      "Current uptime in seconds",
		}),
		threads: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "threads",
			Help:      "Apache thread statuses",
		},
			[]string{"state"},
		),
		workers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "workers",
			Help:      "Apache worker statuses",
		},
			[]string{"state"},
		),
		client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure},
			},
		},
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	e.scrapeFailures.Describe(ch)
	e.accessesTotal.Describe(ch)
	e.kBytesTotal.Describe(ch)
	e.uptime.Describe(ch)
	e.threads.Describe(ch)
	e.workers.Describe(ch)
}

// Split colon separated string into two fields
func splitkv(s string) (string, string) {

	if len(s) == 0 {
		return s, s
	}

	slice := strings.SplitN(s, ":", 2)

	if len(slice) == 1 {
		return slice[0], ""
	}

	return strings.TrimSpace(slice[0]), strings.TrimSpace(slice[1])
}

// Split a row of HTML table
func splitrow(s string) (r []string) {
	if len(s) == 0 {
		return r
	}

	x := strings.Split(s, "<td>")
	for _, v := range x {
		y := strings.Split(v, "</td>")
		if len(y) == 2 {
			r = append(r, strings.TrimSpace(y[0]))
		}
	}
	return r
}

func (e *Exporter) collect(ch chan<- prometheus.Metric) error {
	resp, err := e.client.Get(e.URI)
	if err != nil {
		return fmt.Errorf("Error scraping apache: %v", err)
	}

	data, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		if err != nil {
			data = []byte(err.Error())
		}
		return fmt.Errorf("Status %s (%d): %s", resp.Status, resp.StatusCode, data)
	}

	lines := strings.Split(string(data), "\n")

	for _, l := range lines {
		if strings.Contains(l, "<td>Sum</td>") {
			x := splitrow(l)
			if len(x) == 8 {
				fmt.Println(x)

				val, err := strconv.ParseFloat(x[3], 64)
				if err != nil {
					return err
				}
				e.threads.WithLabelValues("busy").Set(val)

				val, err = strconv.ParseFloat(x[4], 64)
				if err != nil {
					return err
				}
				e.threads.WithLabelValues("idle").Set(val)
			}
			continue
		}

		key, v := splitkv(l)

		switch {
		case key == "Total Accesses":
			val, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return err
			}

			e.accessesTotal.Set(val)
			e.accessesTotal.Collect(ch)
		case key == "Total kBytes":
			val, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return err
			}

			e.kBytesTotal.Set(val)
			e.kBytesTotal.Collect(ch)
		case key == "Uptime":
			val, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return err
			}

			e.uptime.Set(val)
			e.uptime.Collect(ch)
		case key == "BusyWorkers":
			val, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return err
			}

			e.workers.WithLabelValues("busy").Set(val)
		case key == "IdleWorkers":
			val, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return err
			}

			e.workers.WithLabelValues("idle").Set(val)
		}
	}

	e.threads.Collect(ch)
	e.workers.Collect(ch)

	return nil
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock() // To protect metrics from concurrent collects.
	defer e.mutex.Unlock()
	if err := e.collect(ch); err != nil {
		log.Printf("Error scraping apache: %s", err)
		e.scrapeFailures.Inc()
		e.scrapeFailures.Collect(ch)
	}
	return
}

func main() {
	flag.Parse()

	exporter := NewExporter(*scrapeURI)
	prometheus.MustRegister(exporter)

	log.Printf("Starting Server: %s", *listeningAddress)
	http.Handle(*metricsEndpoint, prometheus.Handler())
	log.Fatal(http.ListenAndServe(*listeningAddress, nil))
}
