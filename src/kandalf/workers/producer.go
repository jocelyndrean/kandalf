package workers

import (
	"path"
	"sort"

	log "github.com/Sirupsen/logrus"
	"gopkg.in/Shopify/sarama.v1"

	"../config"
	"../logger"
	"../pipes"
)

type internalProducer struct {
	client    sarama.SyncProducer
	pipesList []pipes.Pipe
}

// Returns new instance of kafka producer
func newInternalProducer() (*internalProducer, error) {
	var brokers []string

	brokersInterfaces, err := config.Instance().List("kafka.brokers")
	if err != nil {
		return nil, err
	} else {
		for _, broker := range brokersInterfaces {
			brokers = append(brokers, broker.(string))
		}
	}

	cnf := sarama.NewConfig()
	cnf.Producer.RequiredAcks = sarama.WaitForAll
	cnf.Producer.Retry.Max = config.Instance().UInt("kafka.retry_max", 5)

	client, err := sarama.NewSyncProducer(brokers, cnf)
	if err != nil {
		return nil, err
	}

	return &internalProducer{
		client:    client,
		pipesList: pipes.All(),
	}, nil
}

// Sends message to the kafka
func (p *internalProducer) handleMessage(msg internalMessage) (err error) {
	topic := getTopic(msg, p.pipesList)
	fields := log.Fields{
		"exchange_name": msg.ExchangeName,
		"routed_queues": msg.RoutedQueues,
		"routing_keys":  msg.RoutingKeys,
	}

	if len(topic) > 0 {
		_, _, err = p.client.SendMessage(&sarama.ProducerMessage{
			Topic: topic,
			Value: sarama.ByteEncoder(msg.Body),
		})

		if err == nil {
			fields["topic"] = topic

			logger.Instance().
				WithFields(fields).
				Debug("Successfully sent message to kafka")
		} else {
			logger.Instance().
				WithFields(fields).
				Debug("Un error occurred while sending message to kafka")
		}
	} else {
		err = nil

		logger.Instance().
			WithFields(fields).
			Warning("Unable to find Kafka topic for message")
	}

	return err
}

// That's what this is all about.
// Find the topic for a message, based on rules in pipes
func getTopic(msg internalMessage, pipesList []pipes.Pipe) string {
	var (
		scores         map[int]int = make(map[int]int)
		pipeMatched    bool
		pipeFound      bool
		foundedPipeIdx int
		nbScores       int
	)

	for position, pipe := range pipesList {
		scores[position] = 0
		nbScores++

		if len(msg.ExchangeName) > 0 && pipe.HasExchangeName {
			pipeMatched, _ = path.Match(pipe.ExchangeName, msg.ExchangeName)
			if pipeMatched {
				scores[position]++
			}
		}

		if len(msg.RoutedQueues) > 0 && pipe.HasRoutedQueue && isAllKeysMatchPattern(msg.RoutedQueues, pipe.RoutedQueue) {
			scores[position]++
		}

		if len(msg.RoutingKeys) > 0 && pipe.HasRoutingKey && isAllKeysMatchPattern(msg.RoutingKeys, pipe.RoutingKey) {
			scores[position]++
		}

		// If score is 3, than pipe satisfies to all message's fields
		if scores[position] == 3 {
			pipeFound = true
			foundedPipeIdx = position
			break
		}
	}

	if !pipeFound && nbScores > 0 {
		var positions []int

		for position := range scores {
			positions = append(positions, position)
		}

		// Sort scores descending
		sort.Sort(sort.Reverse(sort.IntSlice(positions)))

		pipeFound = true
		foundedPipeIdx = positions[0]
	}

	if pipeFound {
		return pipesList[foundedPipeIdx].Topic
	}

	return ""
}

// Checks if all strings match the pattern
func isAllKeysMatchPattern(keys []string, pattern string) bool {
	var matched bool

	for _, key := range keys {
		matched, _ = path.Match(pattern, key)
		if !matched {
			return false
		}
	}

	return true
}
