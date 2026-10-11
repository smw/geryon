package server

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// BuildVersion is the geryon version, set by the main package.
var BuildVersion = "dev"

// metrics is what the server reports to Prometheus on /metrics.
type metrics struct {
	registry *prometheus.Registry

	gamesStarted  *prometheus.CounterVec // by game_id
	gamesEnded    *prometheus.CounterVec // by game_id, reason
	gameDuration  prometheus.Histogram
	startFailures prometheus.Counter

	logins        *prometheus.CounterVec // by method, result
	registrations *prometheus.CounterVec // by result

	connections      prometheus.Counter
	messagesReceived prometheus.Counter
	framesSent       prometheus.Counter
	bytesSent        *prometheus.CounterVec // by stage: before or after compression
	slowClients      prometheus.Counter

	crawlMessages prometheus.Counter
	crawlBytes    prometheus.Counter
	crawlDropped  prometheus.Counter

	chatMessages prometheus.Counter
	reloads      *prometheus.CounterVec // by result
	failures     *prometheus.CounterVec // by component
}

func newMetrics(hub *Hub, gameIDs func() []string) *metrics {
	counter := func(name, help string) prometheus.Counter {
		return prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
	}
	vec := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	}
	m := &metrics{
		registry: prometheus.NewRegistry(),

		gamesStarted: vec("geryon_games_started_total", "Crawl games started.", "game_id"),
		gamesEnded: vec("geryon_games_ended_total",
			"Crawl games ended, by the reason crawl gave (saved, dead, won, disconnect, crash, ...).",
			"game_id", "reason"),
		gameDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "geryon_game_duration_seconds",
			Help:    "How long a play session lasted, from crawl starting to crawl exiting.",
			Buckets: []float64{10, 60, 300, 900, 1800, 3600, 7200, 14400, 28800},
		}),
		startFailures: counter("geryon_game_start_failures_total",
			"Games that ended before crawl was running: cancelled, or failed to start."),

		logins: vec("geryon_logins_total", "Login attempts.", "method", "result"),
		registrations: vec("geryon_registrations_total",
			"Account registration attempts.", "result"),

		connections: counter("geryon_connections_total", "Browser connections accepted."),
		messagesReceived: counter("geryon_websocket_messages_received_total",
			"Messages received from browsers."),
		framesSent: counter("geryon_websocket_frames_sent_total", "Frames sent to browsers."),
		bytesSent: vec("geryon_websocket_sent_bytes_total",
			"Bytes sent to browsers: as JSON (stage=\"raw\") and as put on the wire after compression (stage=\"wire\").",
			"stage"),
		slowClients: counter("geryon_slow_clients_dropped_total",
			"Connections closed because the browser was not keeping up with its game."),

		crawlMessages: counter("geryon_crawl_messages_total", "Messages received from crawl processes."),
		crawlBytes:    counter("geryon_crawl_message_bytes_total", "Bytes of messages received from crawl processes."),
		crawlDropped: counter("geryon_crawl_messages_dropped_total",
			"Messages for a crawl process dropped because it was not reading its socket."),

		chatMessages: counter("geryon_chat_messages_total", "Chat messages relayed."),
		reloads:      vec("geryon_game_list_reloads_total", "Reloads of the game definitions.", "result"),
		failures: vec("geryon_internal_errors_total",
			"Sessions or games ended by a bug in the server (a recovered panic).", "component"),
	}
	m.registry.MustRegister(
		m.gamesStarted, m.gamesEnded, m.gameDuration, m.startFailures,
		m.logins, m.registrations,
		m.connections, m.messagesReceived, m.framesSent, m.bytesSent, m.slowClients,
		m.crawlMessages, m.crawlBytes, m.crawlDropped,
		m.chatMessages, m.reloads, m.failures,
		&hubCollector{hub: hub, gameIDs: gameIDs},
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	info := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "geryon_build_info",
		Help:        "Always 1; the version label is the geryon build.",
		ConstLabels: prometheus.Labels{"version": BuildVersion},
	})
	info.Set(1)
	m.registry.MustRegister(info)
	// So that the common series exist from the start rather than at first use.
	for _, result := range []string{"success", "failure"} {
		m.logins.WithLabelValues("password", result)
		m.logins.WithLabelValues("token", result)
		m.reloads.WithLabelValues(result)
	}
	return m
}

func result(ok bool) string {
	if ok {
		return "success"
	}
	return "failure"
}

func (m *metrics) countLogin(method string, ok bool) {
	m.logins.WithLabelValues(method, result(ok)).Inc()
}

// hubCollector reports the current state of the hub at each scrape.
type hubCollector struct {
	hub     *Hub
	gameIDs func() []string // the configured games
}

var (
	connectionsDesc = prometheus.NewDesc("geryon_connections",
		"Browser connections now, by what they are doing (lobby, playing, watching).", []string{"state"}, nil)
	gamesDesc = prometheus.NewDesc("geryon_games",
		"Crawl games running now.", []string{"game_id"}, nil)
	spectatorsDesc = prometheus.NewDesc("geryon_spectators",
		"Spectators now, across all games.", nil, nil)
	idleGamesDesc = prometheus.NewDesc("geryon_games_idle",
		"Running games whose player has done nothing for 30 seconds.", nil, nil)
)

func (c *hubCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- connectionsDesc
	ch <- gamesDesc
	ch <- spectatorsDesc
	ch <- idleGamesDesc
}

func (c *hubCollector) Collect(ch chan<- prometheus.Metric) {
	st := c.hub.stats()
	for _, state := range []string{"lobby", "playing", "watching"} {
		ch <- prometheus.MustNewConstMetric(connectionsDesc, prometheus.GaugeValue,
			float64(st.connections[state]), state)
	}
	// Every configured game gets a series, at zero when nobody plays it.
	for _, gameID := range c.gameIDs() {
		if _, running := st.games[gameID]; !running {
			st.games[gameID] = 0
		}
	}
	for gameID, n := range st.games {
		ch <- prometheus.MustNewConstMetric(gamesDesc, prometheus.GaugeValue, float64(n), gameID)
	}
	ch <- prometheus.MustNewConstMetric(spectatorsDesc, prometheus.GaugeValue, float64(st.spectators))
	ch <- prometheus.MustNewConstMetric(idleGamesDesc, prometheus.GaugeValue, float64(st.idle))
}
