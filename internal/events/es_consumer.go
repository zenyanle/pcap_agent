package events

import (
	"pcap_agent/pkg/logger"

	"github.com/elastic/go-elasticsearch/v7"
)

// ESConsumer reads events from an Emitter and writes them to Elasticsearch.
// It runs in a background goroutine and stops when the subscribed channel is closed.
type ESConsumer struct {
	es    *elasticsearch.Client
	index string
}

// NewESConsumer creates a consumer that forwards events to ES.
// Call Start() to begin consuming from an emitter.
func NewESConsumer(es *elasticsearch.Client, index string) *ESConsumer {
	if index == "" {
		index = "pcap_agent_events"
	}
	return &ESConsumer{es: es, index: index}
}

// Start begins consuming events from the emitter in a background goroutine.
// Returns immediately. The goroutine exits when the emitter is closed.
func (c *ESConsumer) Start(emitter Emitter) {
	ch := emitter.Subscribe()
	go func() {
		for evt := range ch {
			if err := logger.SendWrappedLog(c.es, c.index, evt.Type, evt); err != nil {
				logger.Warnf("[ESConsumer] failed to write event (type=%s): %v", evt.Type, err)
			}
		}
	}()
}
