package client

import (
	"time"

	"github.com/erkkah/letarette/pkg/protocol"
	"github.com/nats-io/nats.go"
)

// Monitor listens to status broadcasts from a letarette cluster
type Monitor interface {
	Close()
}

// MonitorListener is a callback function receiving status broadcasts
type MonitorListener func(protocol.IndexStatus)

// NewMonitor - Monitor constructor
func NewMonitor(url string, listener MonitorListener, options ...Option) (Monitor, error) {
	nc, err := nats.Connect(url, nats.MaxReconnects(-1), nats.ReconnectWait(time.Millisecond*500))
	if err != nil {
		return nil, err
	}
	ec, err := nats.NewEncodedConn(nc, nats.JSON_ENCODER)

	client := &monitor{
		state: state{
			conn:    ec,
			topic:   "leta",
			onError: func(error) {},
		},
		listener: listener,
	}

	client.state.apply(options)

	_, err = client.conn.Subscribe(client.topic+".status", func(status *protocol.IndexStatus) {
		client.listener(*status)
	})

	if err != nil {
		return nil, err
	}

	return client, nil
}

type monitor struct {
	state
	listener MonitorListener
}

func (m *monitor) Close() {
	m.conn.Close()
}
