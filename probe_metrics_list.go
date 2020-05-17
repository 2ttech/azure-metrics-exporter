package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/remeh/sizedwaitgroup"
	"net/http"
	"time"
)

func probeMetricsListHandler(w http.ResponseWriter, r *http.Request) {
	var err error
	var timeoutSeconds float64
	wg := sizedwaitgroup.New(opts.ConcurrencySubscription)

	startTime := time.Now()

	// If a timeout is configured via the Prometheus header, add it to the request.
	timeoutSeconds, err = getPrometheusTimeout(r, PROBE_METRICS_LIST_TIMEOUT_DEFAULT)
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

	registry, metricGauge := azureInsightMetrics.CreatePrometheusRegistryAndMetricsGauge(settings.Name)
	metricsList := MetricCollectorList{}

	cacheKey := fmt.Sprintf("probeMetricsListHandler::%x", sha256.Sum256([]byte(r.URL.String())))
	if cacheVal, err := cache.Value(cacheKey); err != nil {
		for _, subscription := range settings.Subscriptions {
			wg.Add()
			go func(subscription string) {
				defer wg.Done()
				wgResource := sizedwaitgroup.New(opts.ConcurrencySubscriptionResource)

				// fetch list of resources
				list, err := azureInsightMetrics.ListResources(subscription, settings.Filter)

				if err != nil {
					Logger.Errorln(buildErrorMessageForMetrics(err, settings))
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}

				for list.NotDone() {
					val := list.Value()

					wgResource.Add()
					go func() {
						defer wgResource.Done()
						result, err := azureInsightMetrics.FetchMetrics(ctx, subscription, *val.ID, settings)

						if err == nil {
							Logger.Verbosef("name[%v]subscription[%v] fetched auto metrics for %v", settings.Name, subscription, *val.ID)
							result.SetGauge(&metricsList, settings)
							prometheusMetricRequests.With(prometheus.Labels{
								"subscriptionID": subscription,
								"handler":        PROBE_METRICS_LIST_URL,
								"filter":         settings.Filter,
								"result":         "success",
							}).Inc()
						} else {
							Logger.Verbosef("name[%v]subscription[%v] failed fetching metrics for %v", settings.Name, subscription, *val.ID)
							Logger.Warningln(buildErrorMessageForMetrics(err, settings))

							prometheusMetricRequests.With(prometheus.Labels{
								"subscriptionID": subscription,
								"handler":        PROBE_METRICS_LIST_URL,
								"filter":         settings.Filter,
								"result":         "error",
							}).Inc()
						}
					}()

					if list.NextWithContext(ctx) != nil {
						break
					}
				}

				wgResource.Wait()

				// global stats counter
				prometheusCollectTime.With(prometheus.Labels{
					"subscriptionID": subscription,
					"handler":        PROBE_METRICS_LIST_URL,
					"filter":         settings.Filter,
				}).Observe(time.Now().Sub(startTime).Seconds())

			}(subscription)
		}

		wg.Wait()

		// enable caching if enabled
		if settings.Cache != nil {
			cache.Add(cacheKey, *settings.Cache, metricsList.list)
		}
	} else {
		// use from cache
		metricsList.list = cacheVal.Data().([]MetricCollectorRow)
	}

	metricsList.GaugeSet(metricGauge)

	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}
