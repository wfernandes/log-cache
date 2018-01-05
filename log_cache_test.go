package logcache_test

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"

	"google.golang.org/grpc"

	rpc "code.cloudfoundry.org/go-log-cache/rpc/logcache"
	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"code.cloudfoundry.org/log-cache"
	"code.cloudfoundry.org/log-cache/internal/egress"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("LogCache", func() {
	It("returns data filtered by source ID in 1 node cluster", func() {
		spyMetrics := newSpyMetrics()
		cache := logcache.New(
			logcache.WithAddr("127.0.0.1:0"),
			logcache.WithMetrics(spyMetrics),
		)
		cache.Start()

		writeEnvelopes(cache.Addr(), []*loggregator_v2.Envelope{
			{Timestamp: 1, SourceId: "app-a"},
			{Timestamp: 2, SourceId: "app-b"},
			{Timestamp: 3, SourceId: "app-a"},
			{
				Timestamp: 4,
				SourceId:  "app-a",
				Message: &loggregator_v2.Envelope_Counter{
					Counter: &loggregator_v2.Counter{Name: "some-name"},
				},
			},
			{
				Timestamp: 5,
				SourceId:  "app-a",
				Message: &loggregator_v2.Envelope_Counter{
					Counter: &loggregator_v2.Counter{Name: "some-name"},
				},
			},
		})

		conn, err := grpc.Dial(cache.Addr(), grpc.WithInsecure())
		Expect(err).ToNot(HaveOccurred())
		defer conn.Close()
		client := rpc.NewEgressClient(conn)

		var es []*loggregator_v2.Envelope
		f := func() error {
			resp, err := client.Read(context.Background(), &rpc.ReadRequest{
				SourceId:       "app-a",
				FilterTemplate: `{{if eq .GetCounter.GetName "some-name"}}include{{end}}`,
			})
			if err != nil {
				return err
			}

			if len(resp.Envelopes.Batch) != 2 {
				return fmt.Errorf("expected 2 envelopes, but got %d", len(resp.Envelopes.Batch))
			}

			es = resp.Envelopes.Batch
			return nil
		}
		Eventually(f).Should(BeNil())

		Expect(es[0].Timestamp).To(Equal(int64(4)))
		Expect(es[0].SourceId).To(Equal("app-a"))
		Expect(es[1].Timestamp).To(Equal(int64(5)))
		Expect(es[1].SourceId).To(Equal("app-a"))

		Eventually(spyMetrics.getter("Ingress")).Should(Equal(uint64(5)))
		Eventually(spyMetrics.getter("Egress")).Should(Equal(uint64(2)))
	})

	It("routes envelopes to peers", func() {
		peer := newSpyLogCache()
		peerAddr := peer.start()

		cache := logcache.New(
			logcache.WithAddr("127.0.0.1:0"),
			logcache.WithClustered(0, []string{"my-addr", peerAddr},
				grpc.WithInsecure(),
			),
		)
		cache.Start()

		writeEnvelopes(cache.Addr(), []*loggregator_v2.Envelope{
			// source-0 hashes to 7700738999732113484 (route to node 0)
			{Timestamp: 1, SourceId: "source-0"},
			// source-1 hashes to 15704273932878139171 (route to node 1)
			{Timestamp: 2, SourceId: "source-1"},
			{Timestamp: 3, SourceId: "source-1"},
		})

		Eventually(peer.getEnvelopes).Should(HaveLen(2))
		Expect(peer.getEnvelopes()[0].Timestamp).To(Equal(int64(2)))
		Expect(peer.getEnvelopes()[1].Timestamp).To(Equal(int64(3)))
	})

	It("accepts envelopes from peers", func() {
		cache := logcache.New(
			logcache.WithAddr("127.0.0.1:0"),
			logcache.WithClustered(0, []string{"my-addr", "other-addr"},
				grpc.WithInsecure(),
			),
		)
		cache.Start()

		peerWriter := egress.NewPeerWriter(
			cache.Addr(),
			grpc.WithInsecure(),
		)

		// source-0 hashes to 7700738999732113484 (route to node 0)
		peerWriter.Write(&loggregator_v2.Envelope{SourceId: "source-0", Timestamp: 1})

		conn, err := grpc.Dial(cache.Addr(), grpc.WithInsecure())
		Expect(err).ToNot(HaveOccurred())
		defer conn.Close()
		client := rpc.NewEgressClient(conn)

		var es []*loggregator_v2.Envelope
		f := func() error {
			resp, err := client.Read(context.Background(), &rpc.ReadRequest{
				SourceId: "source-0",
			})
			if err != nil {
				return err
			}

			if len(resp.Envelopes.Batch) != 1 {
				return fmt.Errorf("expected 1 envelopes but got %d", len(resp.Envelopes.Batch))
			}

			es = resp.Envelopes.Batch
			return nil
		}
		Eventually(f).Should(BeNil())

		Expect(es[0].Timestamp).To(Equal(int64(1)))
		Expect(es[0].SourceId).To(Equal("source-0"))
	})

	It("routes query requests to peers", func() {
		peer := newSpyLogCache()
		peerAddr := peer.start()
		peer.readEnvelopes["source-1"] = []*loggregator_v2.Envelope{
			{Timestamp: 99},
			{Timestamp: 101},
		}

		cache := logcache.New(
			logcache.WithAddr("127.0.0.1:0"),
			logcache.WithClustered(0, []string{"my-addr", peerAddr},
				grpc.WithInsecure(),
			),
		)

		cache.Start()

		conn, err := grpc.Dial(cache.Addr(), grpc.WithInsecure())
		Expect(err).ToNot(HaveOccurred())
		defer conn.Close()
		client := rpc.NewEgressClient(conn)

		// source-1 hashes to 15704273932878139171 (route to node 1)
		resp, err := client.Read(context.Background(), &rpc.ReadRequest{
			SourceId:     "source-1",
			StartTime:    99,
			EndTime:      101,
			EnvelopeType: rpc.EnvelopeTypes_LOG,
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(resp.Envelopes.Batch).To(HaveLen(2))

		Eventually(peer.getReadRequests).Should(HaveLen(1))
		req := peer.getReadRequests()[0]
		Expect(req.SourceId).To(Equal("source-1"))
		Expect(req.StartTime).To(Equal(int64(99)))
		Expect(req.EndTime).To(Equal(int64(101)))
		Expect(req.EnvelopeType).To(Equal(rpc.EnvelopeTypes_LOG))
	})
})

func writeEnvelopes(addr string, es []*loggregator_v2.Envelope) {
	conn, err := grpc.Dial(addr, grpc.WithInsecure())
	if err != nil {
		panic(err)
	}

	client := rpc.NewIngressClient(conn)
	_, err = client.Send(context.Background(), &rpc.SendRequest{
		Envelopes: &loggregator_v2.EnvelopeBatch{
			Batch: es,
		},
	})
	if err != nil {
		panic(err)
	}
}

type spyLogCache struct {
	mu            sync.Mutex
	envelopes     []*loggregator_v2.Envelope
	readRequests  []*rpc.ReadRequest
	readEnvelopes map[string][]*loggregator_v2.Envelope
}

func newSpyLogCache() *spyLogCache {
	return &spyLogCache{
		readEnvelopes: make(map[string][]*loggregator_v2.Envelope),
	}
}

func (s *spyLogCache) start() string {
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	srv := grpc.NewServer()
	rpc.RegisterIngressServer(srv, s)
	rpc.RegisterEgressServer(srv, s)
	go srv.Serve(lis)

	return lis.Addr().String()
}

func (s *spyLogCache) getEnvelopes() []*loggregator_v2.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := make([]*loggregator_v2.Envelope, len(s.envelopes))
	copy(r, s.envelopes)
	return r
}

func (s *spyLogCache) getReadRequests() []*rpc.ReadRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := make([]*rpc.ReadRequest, len(s.readRequests))
	copy(r, s.readRequests)
	return r
}

func (s *spyLogCache) Send(ctx context.Context, r *rpc.SendRequest) (*rpc.SendResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range r.Envelopes.Batch {
		s.envelopes = append(s.envelopes, e)
	}

	return &rpc.SendResponse{}, nil
}

func (s *spyLogCache) Read(ctx context.Context, r *rpc.ReadRequest) (*rpc.ReadResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.readRequests = append(s.readRequests, r)

	return &rpc.ReadResponse{
		Envelopes: &loggregator_v2.EnvelopeBatch{
			Batch: s.readEnvelopes[r.GetSourceId()],
		},
	}, nil
}
