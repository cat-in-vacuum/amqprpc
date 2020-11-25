package amqprpc

import (
	"context"
	"errors"
	"sync"

	"github.com/makasim/amqpextra/publisher"

	"time"

	"fmt"

	"github.com/google/uuid"
	"github.com/makasim/amqpextra"
	"github.com/makasim/amqpextra/consumer"
	"github.com/makasim/amqpextra/consumer/middleware"
	"github.com/streadway/amqp"
)

var ErrNotDone = errors.New("amqprpc: call is not done")
var ErrReplyQueueGoneAway = errors.New("amqprpc: reply queue has gone away")
var ErrShutdown = errors.New("amqprpc: client is shut down")

type options struct {
	replyQueue ReplyQueue
	consumer   Consumer

	preFetchCount  int
	workerCount    int
	shutdownPeriod time.Duration
}

type replyQueue struct {
	name    string
	closeCh <-chan struct{}
}

type Client struct {
	opts options

	context    context.Context
	cancelFunc context.CancelFunc

	pool            *pool
	consumerConnCh  <-chan *amqpextra.Connection
	consumer        *consumer.Consumer
	consumerStateCh chan consumer.State

	publisherConnCh    <-chan *amqpextra.Connection
	publisher          *publisher.Publisher
	publisherUnreadyCh chan error

	setReplyQueueCh chan string
	replyQueueCh    chan replyQueue

	closeCallsCh chan struct{}

	closingMutex sync.Mutex
	closing      bool
}

func New(
	consumerConnCh,
	publisherConnCh <-chan *amqpextra.Connection,
	opts ...Option,
) (*Client, error) {
	client := &Client{
		opts: options{
			replyQueue: ReplyQueue{
				Name:       "",
				Declare:    true,
				AutoDelete: true,
				Exclusive:  true,
			},
			consumer: Consumer{
				AutoAck:   true,
				Exclusive: true,
			},
			preFetchCount:  10,
			workerCount:    10,
			shutdownPeriod: 20 * time.Second,
		},

		context: context.Background(),

		consumerConnCh: consumerConnCh,

		publisherConnCh: publisherConnCh,

		closeCallsCh: make(chan struct{}),

		setReplyQueueCh: make(chan string, 1),
		replyQueueCh:    make(chan replyQueue),

		pool: newPool(),
	}

	for _, opt := range opts {
		opt(client)
	}

	handler := consumer.Wrap(
		consumer.HandlerFunc(client.reply),
		middleware.Recover(),
		middleware.AckNack(),
	)

	stateCh := make(chan consumer.State, 1)
	client.consumerStateCh = stateCh
	c, err := amqpextra.NewConsumer(
		consumerConnCh,
		client.opts.resolveConsumerOptions(handler, stateCh)...)
	if err != nil {
		return nil, err
	}

	client.consumer = c

	publisherReadyCh := make(chan struct{}, 1)
	client.publisherUnreadyCh = make(chan error, 1)

	pub, err := amqpextra.NewPublisher(
		publisherConnCh,
		publisher.WithNotify(publisherReadyCh, client.publisherUnreadyCh))
	if err != nil {
		return nil, err
	}
	client.publisher = pub

	client.context, client.cancelFunc = context.WithCancel(client.context)

	go func() {
		closeCh := make(chan struct{})

		for {
			select {
			case consumerState := <-stateCh:
				for {
					if consumerState.Unready != nil {
						break
					}
					select {
					case client.replyQueueCh <- replyQueue{
						name:    consumerState.Ready.Queue,
						closeCh: closeCh,
					}:
					case <-client.consumer.NotifyClosed():
						return
					}
				}
			case <-client.consumer.NotifyClosed():
				return
			}
			if client.opts.replyQueue.Name == "" || client.opts.replyQueue.AutoDelete {
				close(closeCh)
				closeCh = make(chan struct{})
			}
		}
	}()

	go func() {
		select {
		case <-client.context.Done():
			client.Close()
		case <-client.publisherUnreadyCh:
			return
		}
	}()

	return client, nil
}

func (o *options) resolveConsumerOptions(h consumer.Handler, sateCh chan consumer.State) []consumer.Option {
	var (
		ops = []consumer.Option{
			consumer.WithWorker(consumer.NewParallelWorker(o.workerCount)),
			consumer.WithNotify(sateCh),
			consumer.WithQos(o.preFetchCount, false),
			consumer.WithHandler(h),
		}

		declare = o.replyQueue.Declare
		name    = o.replyQueue.Name
	)

	if declare && name == "" {
		ops = append(ops, consumer.WithTmpQueue())
	} else if !declare && name == "" {
		panic("declare flag or queue name for ReplyQueue must be provided in WithReplyQueue")
	}

	if declare && name != "" {
		ops = append(ops, consumer.WithDeclareQueue(
			o.replyQueue.Name,
			o.replyQueue.Durable,
			o.replyQueue.AutoDelete,
			o.replyQueue.Exclusive,
			o.replyQueue.NoWait,
			o.replyQueue.Args,
		))
	}

	if !declare && name != "" {
		ops = append(ops, consumer.WithQueue(name))
	}

	return ops
}

func (client *Client) Go(msg publisher.Message, done chan *Call) *Call {
	call := newCall(msg, done, client.pool, client.opts.consumer.AutoAck)
	go client.send(call)

	return call
}

func (client *Client) Call(msg publisher.Message) (amqp.Delivery, error) {
	doneCh := make(chan *Call, 1)
	call := newCall(msg, doneCh, client.pool, client.opts.consumer.AutoAck)
	client.send(call)

	return call.Delivery()
}

func (client *Client) Close() error {
	client.closingMutex.Lock()
	if client.closing {
		client.closingMutex.Unlock()
		return ErrShutdown
	}

	client.closing = true
	client.closingMutex.Unlock()

	defer client.cancelFunc()
	defer client.consumer.Close()
	defer client.publisher.Close()

	shutdownPeriodTimer := time.NewTimer(client.opts.shutdownPeriod)
	defer shutdownPeriodTimer.Stop()

	client.publisher.Close()
	select {
	case <-client.publisher.NotifyClosed():
	case <-shutdownPeriodTimer.C:
		return fmt.Errorf("amqprpc: shutdown grace period time out: publisher not stopped")
	}

	var result error
	if client.pool.count() > 0 {
		ticker := time.NewTicker(time.Millisecond * 200)
		defer ticker.Stop()

	loop:
		for {
			select {
			case <-ticker.C:
				if client.pool.count() == 0 {
					break loop
				}
			case <-shutdownPeriodTimer.C:
				result = fmt.Errorf("amqprpc: shutdown grace period time out: some calls have not been done")
				shutdownPeriodTimer.Reset(2 * time.Second)

				break loop
			}
		}
	}

	close(client.closeCallsCh)

	client.consumer.Close()
	select {
	case <-client.consumer.NotifyClosed():
	case <-shutdownPeriodTimer.C:
		return fmt.Errorf("amqprpc: shutdown grace period time out: consumer not stopped")
	}

	return result
}

func (client *Client) send(call *Call) {
	var (
		publisherUnreadyCh chan error
		consumerStateCh    chan consumer.State
	)

	if call.message.ErrOnUnready {
		publisherUnreadyCh = client.publisherUnreadyCh
		consumerStateCh = client.consumerStateCh
	}

	if call.message.Context == nil {
		call.message.Context = context.Background()
	}

	select {
	case replyQueue := <-client.replyQueueCh:
		resultCh := make(chan error, 1)
		call.message.Publishing.ReplyTo = replyQueue.name
		call.message.Publishing.CorrelationId = uuid.New().String()
		call.message.ResultCh = resultCh
		client.pool.set(call)
		err := client.publisher.Publish(call.message)
		if err != nil {
			call.errored(err)
		}
		for {
			select {
			case err := <-resultCh:
				if err != nil {
					call.errored(err)
					return
				}

				resultCh = nil
				continue
			case <-call.Closed():
				return
			case <-call.closeCh:
				return
			case <-client.closeCallsCh:
				call.errored(ErrShutdown)
				return
			case <-replyQueue.closeCh:
				call.errored(ErrReplyQueueGoneAway)
				return
			case <-call.message.Context.Done():
				call.errored(call.message.Context.Err())
				return
			}
		}
	case <-call.Closed():
		return
	// noinspection GoNilness
	case state := <-consumerStateCh:
		if state.Unready != nil {
			call.errored(fmt.Errorf("amqprpc: consumer unready: %s", state.Unready.Err))
			return
		}
	// noinspection GoNilness
	case err := <-publisherUnreadyCh:
		call.errored(fmt.Errorf("amqprpc: publisher unready: %s", err))
		return
	case <-call.message.Context.Done():
		call.errored(call.message.Context.Err())
		return
	case <-client.publisher.NotifyClosed():
		call.errored(ErrShutdown)
		return
	}
}

func (client *Client) reply(_ context.Context, msg amqp.Delivery) interface{} {
	if msg.CorrelationId == "" {
		return middleware.Nack
	}

	call, ok := client.pool.fetch(msg.CorrelationId)
	if !ok {
		return middleware.Nack
	}

	if !call.ok(msg) {
		return middleware.Nack
	}

	return middleware.Ack
}
