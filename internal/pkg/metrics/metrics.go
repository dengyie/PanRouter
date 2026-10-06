// Package metrics 是 M1 的极简 Prometheus 文本指标实现(counters/gauges),
// M2 可平滑替换为 client_golang,对外格式不变。
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

type series struct {
	labels string // 形如 `pan="quark",kind="risk_control"`
	value  float64
}

type Registry struct {
	mu        sync.Mutex
	counters  map[string]map[string]*series // name -> labelkey -> series
	gauges    map[string]map[string]*series
	helpLines map[string]string
}

func New() *Registry {
	return &Registry{
		counters:  map[string]map[string]*series{},
		gauges:    map[string]map[string]*series{},
		helpLines: map[string]string{},
	}
}

// Help 注册指标说明(重复调用以最后一次为准)。
func (r *Registry) Help(name, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.helpLines[name] = text
}

func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, escape(labels[k])))
	}
	return strings.Join(parts, ",")
}

func escape(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func (r *Registry) Inc(name string, labels map[string]string) {
	r.Add(name, labels, 1)
}

func (r *Registry) Add(name string, labels map[string]string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.counters[name]
	if !ok {
		m = map[string]*series{}
		r.counters[name] = m
	}
	k := labelKey(labels)
	s, ok := m[k]
	if !ok {
		s = &series{labels: k}
		m[k] = s
	}
	s.value += v
}

func (r *Registry) Set(name string, labels map[string]string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.gauges[name]
	if !ok {
		m = map[string]*series{}
		r.gauges[name] = m
	}
	k := labelKey(labels)
	m[k] = &series{labels: k, value: v}
}

func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := r.snapshot()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(body)
	})
}

func (r *Registry) snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var sb strings.Builder
	writeFamily := func(typ, name string, m map[string]*series) {
		if len(m) == 0 {
			return
		}
		if h, ok := r.helpLines[name]; ok {
			fmt.Fprintf(&sb, "# HELP %s %s\n", name, h)
		}
		fmt.Fprintf(&sb, "# TYPE %s %s\n", name, typ)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := m[k]
			if s.labels == "" {
				fmt.Fprintf(&sb, "%s %v\n", name, s.value)
				continue
			}
			fmt.Fprintf(&sb, "%s{%s} %v\n", name, s.labels, s.value)
		}
	}
	writeFamily("counter", "panrouter_resolve_total", r.counters["panrouter_resolve_total"])
	writeFamily("counter", "panrouter_download_route_total", r.counters["panrouter_download_route_total"])
	writeFamily("counter", "panrouter_link_cache_hit_total", r.counters["panrouter_link_cache_hit_total"])
	writeFamily("counter", "panrouter_driver_error_total", r.counters["panrouter_driver_error_total"])
	writeFamily("gauge", "panrouter_breaker_open", r.gauges["panrouter_breaker_open"])
	writeFamily("gauge", "panrouter_account_status", r.gauges["panrouter_account_status"])
	return []byte(sb.String())
}
