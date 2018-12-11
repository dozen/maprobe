package maprobe

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"

	mackerel "github.com/mackerelio/mackerel-client-go"
)

var (
	MaxConcurrency         = 100
	MaxClientConcurrency   = 5
	PostMetricBufferLength = 100
	sem                    = make(chan struct{}, MaxConcurrency)
	clientSem              = make(chan struct{}, MaxClientConcurrency)
	ProbeInterval          = 60 * time.Second
	mackerelRetryInterval  = 10 * time.Second
	metricTimeMargin       = -3 * time.Minute
)

func lock() {
	sem <- struct{}{}
	log.Printf("[trace] locked. concurrency: %d", len(sem))
}

func unlock() {
	<-sem
	log.Printf("[trace] unlocked. concurrency: %d", len(sem))
}

func Run(ctx context.Context, wg *sync.WaitGroup, configPath string, once bool) error {
	defer wg.Done()
	defer log.Println("[info] stopping maprobe")

	log.Println("[info] starting maprobe")
	conf, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	log.Println("[debug]", conf.String())
	client := mackerel.NewClient(conf.APIKey)

	hch := make(chan HostMetric, PostMetricBufferLength*10)
	defer close(hch)
	sch := make(chan ServiceMetric, PostMetricBufferLength*10)
	defer close(sch)

	if len(conf.Probes) > 0 {
		if conf.PostProbedMetrics {
			wg.Add(1)
			go postHostMetricWorker(wg, client, hch)
		} else {
			go dumpHostMetricWorker(hch)
		}
	}

	if len(conf.Aggregates) > 0 {
		if conf.PostAggregatedMetrics {
			wg.Add(1)
			go postServiceMetricWorker(wg, client, sch)
		} else {
			go dumpServiceMetricWorker(sch)
		}
	}

	ticker := time.NewTicker(ProbeInterval)
	for {
		var wg2 sync.WaitGroup
		for _, pd := range conf.Probes {
			wg2.Add(1)
			go runProbes(ctx, pd, client, hch, &wg2)
		}
		for _, ag := range conf.Aggregates {
			wg2.Add(1)
			go runAggregates(ctx, ag, client, sch, &wg2)
		}
		wg2.Wait()
		if once {
			return nil
		}

		log.Println("[debug] waiting for a next tick")
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		log.Println("[debug] checking a new config")
		newConf, err := LoadConfig(configPath)
		if err != nil {
			log.Println("[warn]", err)
			log.Println("[warn] still using current config")
		} else if !reflect.DeepEqual(conf, newConf) {
			conf = newConf
			log.Println("[info] config reloaded")
			log.Println("[debug]", conf)
		}
	}
	return nil
}

func runProbes(ctx context.Context, pd *ProbeDefinition, client *mackerel.Client, ch chan HostMetric, wg *sync.WaitGroup) {
	defer wg.Done()
	log.Printf(
		"[debug] probes finding hosts service:%s roles:%s statuses:%v",
		pd.Service,
		pd.Roles,
		pd.Statuses,
	)
	hosts, err := client.FindHosts(&mackerel.FindHostsParam{
		Service:  pd.Service,
		Roles:    pd.Roles,
		Statuses: pd.Statuses,
	})
	if err != nil {
		log.Println("[error] probes find host failed", err)
		return
	}
	log.Printf("[debug] probes %d hosts found", len(hosts))
	if len(hosts) == 0 {
		return
	}

	spawnInterval := time.Duration(int64(ProbeInterval) / int64(len(hosts)) / 2)
	if spawnInterval > time.Second {
		spawnInterval = time.Second
	}

	var wg2 sync.WaitGroup
	for _, host := range hosts {
		time.Sleep(spawnInterval)
		log.Printf("[debug] probes preparing host id:%s name:%s", host.ID, host.Name)
		wg2.Add(1)
		go func(host *mackerel.Host) {
			lock()
			defer unlock()
			defer wg2.Done()
			for _, probe := range pd.GenerateProbes(host, client) {
				log.Printf("[debug] probing host id:%s name:%s probe:%s", host.ID, host.Name, probe)
				metrics, err := probe.Run(ctx)
				if err != nil {
					log.Printf("[warn] probe failed. %s host id:%s name:%s probe:%s", err, host.ID, host.Name, probe)
				}
				for _, m := range metrics {
					ch <- m
				}
			}
		}(host)
	}
	wg2.Wait()
}

func runAggregates(ctx context.Context, ag *AggregateDefinition, client *mackerel.Client, ch chan ServiceMetric, wg *sync.WaitGroup) {
	defer wg.Done()
	log.Printf(
		"[debug] aggregates finding hosts service:%s roles:%s statuses:%v",
		ag.Service,
		ag.Roles,
		ag.Statuses,
	)
	hosts, err := client.FindHosts(&mackerel.FindHostsParam{
		Service:  ag.Service,
		Roles:    ag.Roles,
		Statuses: ag.Statuses,
	})
	if err != nil {
		log.Println("[error] aggregates find hosts failed", err)
		return
	}
	log.Printf("[debug] aggregates %d hosts found", len(hosts))
	if len(hosts) == 0 {
		return
	}
	hostIDs := make([]string, 0, len(hosts))
	for _, h := range hosts {
		hostIDs = append(hostIDs, h.ID)
	}
	metricNames := make([]string, 0, len(ag.Metrics))
	for _, m := range ag.Metrics {
		metricNames = append(metricNames, m.Name)
	}

	log.Printf("[debug] fetching latest metrics hosts:%v metrics:%v", hostIDs, metricNames)
	latest, err := fetchLatestMetricValues(client, hostIDs, metricNames)
	if err != nil {
		log.Printf("[error] fetch latest metrics failed. %s hosts:%v metrics:%v", err, hostIDs, metricNames)
		return
	}

	now := time.Now()
	for _, mc := range ag.Metrics {
		name := mc.Name
		var timestamp float64
		values := []float64{}
		for hostID, metrics := range latest {
			if _v, ok := metrics[name]; ok {
				if _v == nil {
					log.Printf("[debug] latest %s:%s is not found", hostID, name)
					continue
				}
				v, ok := _v.Value.(float64)
				if !ok {
					log.Printf("[warn] latest %s:%s = %v is not a float64 value", hostID, name, _v)
					continue
				}
				ts := time.Unix(_v.Time, 0)
				log.Printf("[debug] latest %s:%s:%d = %f", hostID, name, _v.Time, v)
				if ts.After(now.Add(metricTimeMargin)) {
					values = append(values, v)
					timestamp = math.Max(float64(_v.Time), timestamp)
				} else {
					log.Printf("[warn] latest %s:%s at %s is outdated", hostID, name, ts)
				}
			}
		}
		if len(values) == 0 {
			continue
		}

		for _, output := range mc.Outputs {
			var value float64
			switch strings.ToLower(output.Func) {
			case "sum":
				value = sum(values)
			case "min":
				value = min(values)
			case "max":
				value = max(values)
			case "avg", "average":
				value = avg(values)
			case "count":
				value = count(values)
			}
			log.Printf("[debug] aggregates %s(%s)=%f -> %s:%s",
				output.Func, name,
				value,
				ag.Service, output.Name,
			)
			ch <- ServiceMetric{
				Service:   ag.Service,
				Name:      output.Name,
				Value:     value,
				Timestamp: time.Unix(int64(timestamp), 0),
			}
		}
	}
}

func sum(values []float64) (value float64) {
	for _, v := range values {
		value = value + v
	}
	return
}

func min(values []float64) (value float64) {
	for _, v := range values {
		value = math.Min(v, value)
	}
	return
}

func max(values []float64) (value float64) {
	for _, v := range values {
		value = math.Max(v, value)
	}
	return
}

func count(values []float64) (value float64) {
	return float64(len(values))
}

func avg(values []float64) (value float64) {
	return sum(values) / count(values)
}

func postHostMetricWorker(wg *sync.WaitGroup, client *mackerel.Client, ch chan HostMetric) {
	defer wg.Done()
	ticker := time.NewTicker(10 * time.Second)
	mvs := make([]*mackerel.HostMetricValue, 0, PostMetricBufferLength)
	run := true
	for run {
		select {
		case m, cont := <-ch:
			if cont {
				mvs = append(mvs, m.HostMetricValue())
				if len(mvs) < PostMetricBufferLength {
					continue
				}
			} else {
				log.Println("[debug] shutting down postMetricWorker")
				run = false
			}
		case <-ticker.C:
		}
		if len(mvs) == 0 {
			continue
		}
		log.Printf("[debug] posting %d metrics to Mackerel", len(mvs))
		b, _ := json.Marshal(mvs)
		log.Println("[debug]", string(b))
		if err := client.PostHostMetricValues(mvs); err != nil {
			log.Println("[error] failed to post metrics to Mackerel", err)
			time.Sleep(mackerelRetryInterval)
			continue
		}
		log.Printf("[debug] post succeeded.")
		// success. reset buffer
		mvs = mvs[:0]
	}
}

func postServiceMetricWorker(wg *sync.WaitGroup, client *mackerel.Client, ch chan ServiceMetric) {
	defer wg.Done()
}

func dumpHostMetricWorker(ch chan HostMetric) {
	for m := range ch {
		b, _ := json.Marshal(m.HostMetricValue())
		log.Printf("[info] %s %s", m.HostID, b)
	}
}

func dumpServiceMetricWorker(ch chan ServiceMetric) {
	for m := range ch {
		b, _ := json.Marshal(m.MetricValue())
		log.Printf("[info] %s %s", m.Service, b)
	}
}

type templateParam struct {
	Host *mackerel.Host
}
