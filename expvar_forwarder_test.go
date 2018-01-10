package logcache_test

import (
	"net/http"
	"net/http/httptest"
	"time"

	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"code.cloudfoundry.org/log-cache"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("ExpvarForwarder", func() {
	var (
		r *logcache.ExpvarForwarder

		addr     string
		server1  *httptest.Server
		server2  *httptest.Server
		logCache *spyLogCache
	)

	BeforeEach(func() {
		server1 = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`
			{
				"logCache": {
					"cachePeriod": 68644,
					"egress": 999,
					"expired": 0,
					"ingress": 633
				}
			}`))
		}))

		server2 = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`
			{
				"logCache": {
					"egress": 999,
					"ingress": 633
				}
			}`))
		}))

		logCache = newSpyLogCache()
		addr = logCache.start()

		r = logcache.NewExpvarForwarder(addr,
			logcache.WithExpvarInterval(time.Millisecond),
			logcache.AddExpvarGaugeTemplate(
				server1.URL,
				"CachePeriod",
				"mS",
				"log-cache",
				"{{.logCache.cachePeriod}}",
				map[string]string{"a": "some-value"},
			),
			logcache.AddExpvarCounterTemplate(
				server2.URL,
				"Egress",
				"log-cache-nozzle",
				"{{.logCache.egress}}",
				map[string]string{"a": "some-value"},
			),
		)

		go r.Start()
	})

	It("writes the expvar metrics to LogCache", func() {
		Eventually(func() int {
			return len(logCache.getEnvelopes())
		}).Should(BeNumerically(">=", 2))

		var e *loggregator_v2.Envelope

		// Find counter
		for _, ee := range logCache.getEnvelopes() {
			if ee.GetCounter() == nil {
				continue
			}

			e = ee
		}

		Expect(e).ToNot(BeNil())
		Expect(e.SourceId).To(Equal("log-cache-nozzle"))
		Expect(e.Timestamp).ToNot(BeZero())
		Expect(e.GetCounter().Name).To(Equal("Egress"))
		Expect(e.GetCounter().Total).To(Equal(uint64(999)))
		Expect(e.Tags).To(Equal(map[string]string{"a": "some-value"}))

		e = nil
		// Find gauge
		for _, ee := range logCache.getEnvelopes() {
			if ee.GetGauge() == nil {
				continue
			}

			e = ee
		}

		Expect(e).ToNot(BeNil())
		Expect(e.SourceId).To(Equal("log-cache"))
		Expect(e.Timestamp).ToNot(BeZero())
		Expect(e.GetGauge().Metrics).To(HaveLen(1))
		Expect(e.GetGauge().Metrics["CachePeriod"].Value).To(Equal(68644.0))
		Expect(e.GetGauge().Metrics["CachePeriod"].Unit).To(Equal("mS"))
		Expect(e.Tags).To(Equal(map[string]string{"a": "some-value"}))
	})

	It("panics if there is not a counter or gauge configured", func() {
		Expect(func() {
			logcache.NewExpvarForwarder(addr)
		}).To(Panic())
	})

	It("panics if a counter or gauge template is invalid", func() {
		Expect(func() {
			logcache.NewExpvarForwarder(addr,
				logcache.AddExpvarCounterTemplate(
					server1.URL, "some-name", "a", "{{invalid", nil,
				),
			)
		}).To(Panic())

		Expect(func() {
			logcache.NewExpvarForwarder(addr,
				logcache.AddExpvarGaugeTemplate(
					server1.URL, "some-name", "", "a", "{{invalid", nil,
				),
			)
		}).To(Panic())
	})
})
