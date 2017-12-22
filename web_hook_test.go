package logcache_test

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	gologcache "code.cloudfoundry.org/go-log-cache"
	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"code.cloudfoundry.org/log-cache"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("WebHook", func() {
	var (
		r      *spyReader
		reqs   chan *http.Request
		bodies chan []byte
		errs   chan error
		h      *logcache.WebHook
	)

	BeforeEach(func() {
		errs = make(chan error, 100)
	})

	Context("with successful Post", func() {
		BeforeEach(func() {
			r = newSpyReader()

			reqs = make(chan *http.Request, 100)
			bodies = make(chan []byte, 100)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := ioutil.ReadAll(r.Body)
				if err != nil {
					panic(err)
				}

				reqs <- r
				bodies <- b
			}))

			r.errs = []error{nil, nil}
			r.results = [][]*loggregator_v2.Envelope{
				{buildCounter(int64(time.Second), "some-name", 99)},
				{buildGauge(2*int64(time.Second), "some-name", 101)},
			}

			h = logcache.NewWebHook(
				"some-id",
				fmt.Sprintf(`
			{{ if eq .GetCounter.GetName "some-name" }}
				{{$m:=mapInit "metric" .GetCounter.GetName}}
				{{$m.Add "host" "bosh-lite.com"}}
				{{$m.Add "type" "gauge"}}
				{{$m.Add "points" (sliceInit (nsToTime .Timestamp).Unix .GetCounter.GetTotal)}}
				{{$s:=mapInit "series" (sliceInit $m)}}
				{{post %q (mapInit "my-header" "my-value") $s}}
			{{end}}
			`, server.URL),
				r.read,
				logcache.WithWebHookErrorHandler(func(e error) {
					panic(e)
				}),
			)
			go h.Start()
		})

		It("reads from about an hour ago, for the given sourceID", func() {
			Eventually(r.Starts).ShouldNot(BeEmpty())
			Expect(time.Since(r.Starts()[0])).To(BeNumerically("~", time.Hour, 3*time.Second))
			Expect(r.SourceIDs()[0]).To(Equal("some-id"))
		})

		It("uses the provided template to post", func() {
			var r *http.Request
			Eventually(reqs).Should(Receive(&r))
			Expect(r.Method).To(Equal("POST"))
			Expect(r.Header.Get("my-header")).To(Equal("my-value"))
			Eventually(bodies).Should(Receive(MatchJSON(`{"series":[{"host":"bosh-lite.com","metric":"some-name","points":[1,99],"type":"gauge"}]}`)))
		})
	})

	Context("with unsuccessful post", func() {
		BeforeEach(func() {
			r.errs = []error{nil, nil}
			r.results = [][]*loggregator_v2.Envelope{
				{buildCounter(1, "some-name", 99)},
				{buildGauge(2, "some-name", 101)},
			}

			h = logcache.NewWebHook(
				"some-id",
				fmt.Sprintf(`
			{{ if eq .GetCounter.GetName "some-name" }}
				{{post %q nil (mapInit "series" (sliceInit (mapInit "metric" .GetCounter.GetName | mapAdd "host" "bosh-lite.com" | mapAdd "type" "gauge" | mapAdd "points" (sliceAppend nil .Timestamp .GetCounter.GetTotal) ) )) }}
			{{end}}
			`, "http://invalid.url"),
				r.read,
				logcache.WithWebHookErrorHandler(func(e error) {
					errs <- e
				}),
			)
			go h.Start()
		})

		It("invokes error handler with error", func() {
			Eventually(errs).ShouldNot(BeEmpty())
		})
	})

	Context("with non 200 response", func() {
		BeforeEach(func() {
			r.errs = []error{nil, nil}
			r.results = [][]*loggregator_v2.Envelope{
				{buildCounter(1, "some-name", 99)},
				{buildGauge(2, "some-name", 101)},
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(500)
			}))

			h = logcache.NewWebHook(
				"some-id",
				fmt.Sprintf(`
			{{ if eq .GetCounter.GetName "some-name" }}
				{{post %q nil (mapInit "series" (sliceInit (mapInit "metric" .GetCounter.GetName | mapAdd "host" "bosh-lite.com" | mapAdd "type" "gauge" | mapAdd "points" (sliceInit .Timestamp .GetCounter.GetTotal) ) )) }}
			{{end}}
			`, server.URL),
				r.read,
				logcache.WithWebHookErrorHandler(func(e error) {
					errs <- e
				}),
			)
			go h.Start()
		})

		It("invokes error handler with error", func() {
			Eventually(errs).ShouldNot(BeEmpty())
		})
	})

	Context("with failed execute", func() {
		BeforeEach(func() {
			r.errs = []error{nil, nil}
			r.results = [][]*loggregator_v2.Envelope{
				{buildCounter(1, "some-name", 99)},
				{buildGauge(2, "some-name", 101)},
			}

			h = logcache.NewWebHook(
				"some-id",
				`{{.Invalid}}`,
				r.read,
				logcache.WithWebHookErrorHandler(func(e error) {
					errs <- e
				}),
			)
			go h.Start()
		})

		It("invokes error handler with error", func() {
			Eventually(errs).Should(Receive())
		})
	})
})

type spyReader struct {
	mu        sync.Mutex
	sourceIDs []string
	starts    []time.Time
	opts      [][]gologcache.ReadOption

	results [][]*loggregator_v2.Envelope
	errs    []error
}

func newSpyReader() *spyReader {
	return &spyReader{}
}

func (s *spyReader) read(sourceID string, start time.Time, opts ...gologcache.ReadOption) ([]*loggregator_v2.Envelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sourceIDs = append(s.sourceIDs, sourceID)
	s.starts = append(s.starts, start)
	s.opts = append(s.opts, opts)

	if len(s.results) != len(s.errs) {
		panic("results and errs must have same length")
	}

	if len(s.results) == 0 {
		return nil, nil
	}

	defer func() {
		s.results = s.results[1:]
		s.errs = s.errs[1:]
	}()

	return s.results[0], s.errs[0]
}

func (s *spyReader) SourceIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, len(s.sourceIDs))
	copy(ids, s.sourceIDs)
	return ids
}

func (s *spyReader) Starts() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()

	starts := make([]time.Time, len(s.starts))
	copy(starts, s.starts)
	return starts
}

func buildCounter(timestamp int64, name string, total uint64) *loggregator_v2.Envelope {
	return &loggregator_v2.Envelope{
		Timestamp: timestamp,
		Message: &loggregator_v2.Envelope_Counter{
			Counter: &loggregator_v2.Counter{
				Name:  name,
				Total: total,
			},
		},
	}
}

func buildGauge(timestamp int64, name string, value float64) *loggregator_v2.Envelope {
	return &loggregator_v2.Envelope{
		Timestamp: timestamp,
		Message: &loggregator_v2.Envelope_Gauge{
			Gauge: &loggregator_v2.Gauge{
				Metrics: map[string]*loggregator_v2.GaugeValue{
					name: {Value: value},
				},
			},
		},
	}
}
