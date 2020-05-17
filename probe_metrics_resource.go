package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
	"time"
)

func probeMetricsResourceHandler(w http.ResponseWriter, r *http.Request) {
	var err error
	var timeoutSeconds float64

	startTime := time.Now()

	// If a timeout is configured via the Prometheus header, add it to the request.
	timeoutSeconds, err = getPrometheusTimeout(r, PROBE_METRICS_RESOURCE_TIMEOUT_DEFAULT)
	if err != nil {
		Logger.Error(err)
		http.Error(w, fmt.Sprintf("Failed to parse timeout from Prometheus header: %s", err), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds*float64(time.Second)))
	defer cancel()
	r = r.WithContext(ctx)

	var settings RequestMetricSettings
	if settings, err = NewRequestMetricSettings(r); err != nil {
		Logger.Errorln(buildErrorMessageForMetrics(err, settings))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if len(settings.Subscriptions) != 1 {
		Logger.Errorln(buildErrorMessageForMetrics(err, settings))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	subscription := settings.Subscriptions[0]

	if len(settings.Target) == 0 {
		Logger.Errorln(buildErrorMessageForMetrics(err, settings))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	registry, metricGauge := azureInsightMetrics.CreatePrometheusRegistryAndMetricsGauge(settings.Name)

	metricsList := MetricCollectorList{}

	cacheKey := fmt.Sprintf("probeMetricsResourceHandler::%x", sha256.Sum256([]byte(r.URL.String())))
	if cacheVal, err := cache.Value(cacheKey); err != nil {
		for _, target := range settings.Target {
			result, err := azureInsightMetrics.FetchMetrics(ctx, subscription, target, settings)

			if err != nil {
				Logger.Warningln(buildErrorMessageForMetrics(err, settings))
				prometheusMetricRequests.With(prometheus.Labels{
					"subscriptionID": subscription,
					"handler":        PROBE_METRICS_RESOURCE_URL,
					"filter":         "",
					"result":         "error",
				}).Inc()
				http.Error(w, err.Error(), http.StatusBadRequest)
				continue
			}

			Logger.Verbosef("subscription[%v] fetched metrics for %v", subscription, target)
			prometheusMetricRequests.With(prometheus.Labels{
				"subscriptionID": subscription,
				"handler":        PROBE_METRICS_RESOURCE_URL,
				"filter":         "",
				"result":         "success",
			}).Inc()
			result.SetGauge(&metricsList, settings)
		}

		// enable caching if enabled
		if settings.Cache != nil {
			cache.Add(cacheKey, *settings.Cache, metricsList.list)
		}
	} else {
		// use from cache
		metricsList.list = cacheVal.Data().([]MetricCollectorRow)
	}

	metricsList.GaugeSet(metricGauge)

	// global stats counter
	prometheusCollectTime.With(prometheus.Labels{
		"subscriptionID": subscription,
		"handler":        PROBE_METRICS_RESOURCE_URL,
		"filter":         "",
	}).Observe(time.Now().Sub(startTime).Seconds())

	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}
