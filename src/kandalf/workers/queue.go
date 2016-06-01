package workers

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"gopkg.in/redis.v3"

	"../config"
	"../logger"
)

type internalMessage struct {
	Exchange   string          `json:"exchange"`
	RoutingKey string          `json:"routing_key"`
	Body       json.RawMessage `json:"body"`
}

type internalQueue struct {
	flushTimeout   time.Duration
	isWorking      bool
	maxSize        int
	messages       []internalMessage
	mutex          *sync.Mutex
	producer       *internalProducer
	rd             *redis.Client
	rdFlushTimeout time.Duration
	rdKeyName      string
}

// Returns new instance of queue
func newInternalQueue() (*internalQueue, error) {
	var err error

	rdAddr, err := config.Instance().String("queue.redis.address")
	if err != nil {
		return nil, err
	}

	rdClient := redis.NewClient(&redis.Options{
		Addr: rdAddr,
	})

	_, err = rdClient.Ping().Result()
	if err != nil {
		return nil, err
	}

	producer, err := newInternalProducer()
	if err != nil {
		return nil, fmt.Errorf("An error occured while instantiating producer: %v", err)
	}

	return &internalQueue{
		flushTimeout:   time.Duration(config.Instance().UInt("queue.flush_timeout", 5)) * time.Second,
		maxSize:        config.Instance().UInt("queue.max_size", 10),
		messages:       make([]internalMessage, 0),
		mutex:          &sync.Mutex{},
		producer:       producer,
		rd:             rdClient,
		rdFlushTimeout: time.Duration(config.Instance().UInt("queue.redis.flush_timeout", 10)) * time.Second,
		rdKeyName:      config.Instance().UString("queue.redis.key_name", "failed_messages"),
	}, nil
}

// Main working cycle
func (q *internalQueue) run(wg *sync.WaitGroup, die chan bool) {
	defer wg.Done()

	q.isWorking = true

	// Periodically flush the queue to the kafka
	go func() {
		for {
			select {
			case <-die:
				q.isWorking = false
				// Before exiting the working cycle, try to flush the messages to kafka
				q.handleMessages()
				return
			default:
			}

			if len(q.messages) >= q.maxSize {
				q.handleMessages()
			} else {
				time.Sleep(q.flushTimeout)
			}
		}
	}()

	// And move messages from redis back to normal queue
	go q.flushRedis()
}

// Adds message to the internal queue which will be flushed to kafka later
func (q *internalQueue) add(msg internalMessage) {
	q.mutex.Lock()
	defer q.mutex.Unlock()

	q.messages = append(q.messages, msg)
}

// Tries to send messages to the kafka
// If message failed, it will be stored in redis
func (q *internalQueue) handleMessages() {
	q.mutex.Lock()
	defer q.mutex.Unlock()

	var (
		err            error
		failedMessages []internalMessage = make([]internalMessage, 0)
	)

	for _, msg := range q.messages {
		err = q.producer.handleMessage(msg)
		if err == nil {
			continue
		}

		err = q.storeInRedis(msg)

		// If it is not possible to store message even in redis,
		// we'll put the message into memory and process it later
		if err != nil {
			failedMessages = append(failedMessages, msg)
		}
	}

	q.messages = failedMessages
}

// Stores the failed message in redis
func (q *internalQueue) storeInRedis(msg internalMessage) error {
	strMsg, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	return q.rd.LPush(q.rdKeyName, string(strMsg)).Err()
}

// Periodically move messages from Redis back to the main queue
func (q *internalQueue) flushRedis() {
	var (
		err      error
		bytesMsg []byte
		msg      internalMessage
	)

	for q.isWorking {
		for {
			bytesMsg, err = q.rd.LPop(q.rdKeyName).Bytes()
			if err != nil || len(bytesMsg) == 0 {
				break
			}

			err = json.Unmarshal(bytesMsg, &msg)
			if err != nil {
				logger.Instance().
					WithError(err).
					Warning("Unable to unmarshal JSON from redis")
				continue
			}

			q.add(msg)
		}

		time.Sleep(q.rdFlushTimeout)
	}

	// Close connection to redis
	err = q.rd.Close()
	if err != nil {
		logger.Instance().
			WithError(err).
			Warning("An error occurred while closing connection to redis")
	}
}
