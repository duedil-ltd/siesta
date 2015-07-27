package siesta

import (
	"fmt"
	"log"
	"sync"
	"time"
)

type ProducerRecord struct {
	Topic string
	Key   interface{}
	Value interface{}

	metadataChan chan *RecordMetadata
	partition    int32
	encodedKey   []byte
	encodedValue []byte
}

type RecordMetadata struct {
	Offset    int64
	Topic     string
	Partition int32
	Error     error
}

type PartitionInfo struct{}
type Metric struct{}
type ProducerConfig struct {
	MetadataFetchTimeout int64
	MetadataExpireMs     int64
	MaxRequestSize       int
	TotalMemorySize      int
	CompressionType      string
	BatchSize            int
	LingerMs             int64
	RetryBackoffMs       int64
	BlockOnBufferFull    bool

	ClientID        string
	MaxRequests     int
	SendRoutines    int
	ReceiveRoutines int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	RequiredAcks    int
}

type Serializer func(interface{}) ([]byte, error)

func ByteSerializer(value interface{}) ([]byte, error) {
	if value == nil {
		return nil, nil
	}

	if array, ok := value.([]byte); ok {
		return array, nil
	}

	return nil, fmt.Errorf("Can't serialize %v", value)
}

func StringSerializer(value interface{}) ([]byte, error) {
	if str, ok := value.(string); ok {
		return []byte(str), nil
	}

	return nil, fmt.Errorf("Can't serialize %v to string", value)
}

type Producer interface {
	// Send the given record asynchronously and return a channel which will eventually contain the response information.
	Send(*ProducerRecord) <-chan *RecordMetadata

	// Flush any accumulated records from the producer. Blocks until all sends are complete.
	Flush()

	// Get a list of partitions for the given topic for custom partition assignment. The partition metadata will change
	// over time so this list should not be cached.
	PartitionsFor(topic string) []PartitionInfo

	// Return a map of metrics maintained by the producer
	Metrics() map[string]Metric

	// Tries to close the producer cleanly within the specified timeout. If the close does not complete within the
	// timeout, fail any pending send requests and force close the producer.
	Close(timeout int)
}

type KafkaProducer struct {
	config                 *ProducerConfig
	time                   time.Time
	partitioner            Partitioner
	keySerializer          Serializer
	valueSerializer        Serializer
	metadataFetchTimeoutMs int64
	metadata               *Metadata
	maxRequestSize         int
	totalMemorySize        int
	metrics                map[string]Metric
	compressionType        string
	accumulator            *RecordAccumulator
	metricTags             map[string]string
	connector              Connector
	topicMetadataLock      sync.Mutex
	metadataCache          *topicMetadataCache
}

func NewKafkaProducer(config *ProducerConfig, keySerializer Serializer, valueSerializer Serializer, connector Connector) *KafkaProducer {
	log.Println("Starting the Kafka producer")
	producer := &KafkaProducer{}
	producer.config = config
	producer.time = time.Now()
	producer.metrics = make(map[string]Metric)
	producer.partitioner = NewHashPartitioner()
	producer.keySerializer = keySerializer
	producer.valueSerializer = valueSerializer
	producer.metadataFetchTimeoutMs = config.MetadataFetchTimeout
	producer.metadata = NewMetadata()
	producer.maxRequestSize = config.MaxRequestSize
	producer.totalMemorySize = config.TotalMemorySize
	producer.compressionType = config.CompressionType
	producer.connector = connector
	producer.metadataCache = newTopicMetadataCache(connector, time.Duration(config.MetadataExpireMs)*time.Millisecond) //TODO we should probably accept configs in time.Duration and not like BlaBlaMs
	metricTags := make(map[string]string)

	networkClientConfig := NetworkClientConfig{}
	client := NewNetworkClient(networkClientConfig, connector, config)

	accumulatorConfig := &RecordAccumulatorConfig{
		batchSize:         config.BatchSize,
		totalMemorySize:   producer.totalMemorySize,
		compressionType:   producer.compressionType,
		lingerMs:          config.LingerMs,
		blockOnBufferFull: config.BlockOnBufferFull,
		metrics:           producer.metrics,
		time:              producer.time,
		metricTags:        metricTags,
		networkClient:     client,
	}
	producer.accumulator = NewRecordAccumulator(accumulatorConfig)

	log.Println("Kafka producer started")

	return producer
}

func (kp *KafkaProducer) Send(record *ProducerRecord) <-chan *RecordMetadata {
	metadata := make(chan *RecordMetadata, 1)
	kp.send(record, metadata)
	return metadata
}

func (kp *KafkaProducer) send(record *ProducerRecord, metadataChan chan *RecordMetadata) {
	metadata := new(RecordMetadata)

	serializedKey, err := kp.keySerializer(record.Key)
	if err != nil {
		metadata.Error = err
		metadataChan <- metadata
		return
	}

	serializedValue, err := kp.valueSerializer(record.Value)
	if err != nil {
		metadata.Error = err
		metadataChan <- metadata
		return
	}

	record.encodedKey = serializedKey
	record.encodedValue = serializedValue

	partitions, err := kp.metadataCache.Get(record.Topic)
	if err != nil {
		metadata.Error = err
		metadataChan <- metadata
		return
	}

	partition, err := kp.partitioner.Partition(record, partitions)
	if err != nil {
		metadata.Error = err
		metadataChan <- metadata
		return
	}
	record.partition = partition
	record.metadataChan = metadataChan

	kp.accumulator.addChan <- record
}

//func (kp *KafkaProducer) SendCallback(ProducerRecord, Callback) <-chan RecordMetadata {
//	return make(chan RecordMetadata)
//}

func (kp *KafkaProducer) Flush() {}

func (kp *KafkaProducer) PartitionsFor(topic string) []PartitionInfo {
	return []PartitionInfo{}
}

func (kp *KafkaProducer) Metrics() map[string]Metric {
	return make(map[string]Metric)
}

func (kp *KafkaProducer) Close(timeout int) {

	kp.accumulator.close()
}

type topicMetadataCache struct {
	connector   Connector
	ttl         time.Duration
	cache       map[string]*topicMetadataCacheEntry
	refreshLock sync.Mutex
}

func newTopicMetadataCache(connector Connector, ttl time.Duration) *topicMetadataCache {
	return &topicMetadataCache{
		connector: connector,
		ttl:       ttl,
		cache:     make(map[string]*topicMetadataCacheEntry),
	}
}

func (tmc *topicMetadataCache) Get(topic string) ([]int32, error) {
	cache := tmc.cache[topic]
	if cache == nil {
		err := tmc.Refresh([]string{topic})
		if err != nil {
			return nil, err
		}
	}

	cache = tmc.cache[topic]
	if cache != nil {
		if cache.timestamp.Add(tmc.ttl).Before(time.Now()) {
			err := tmc.Refresh([]string{topic})
			if err != nil {
				return nil, err
			}
		}

		cache = tmc.cache[topic]
		if cache != nil {
			return cache.partitions, nil
		}
	}

	return nil, fmt.Errorf("Could not get topic metadata for topic %s", topic)
}

func (tmc *topicMetadataCache) Refresh(topics []string) error {
	tmc.refreshLock.Lock()
	defer tmc.refreshLock.Unlock()

	topicMetadataResponse, err := tmc.connector.GetTopicMetadata(topics)
	if err != nil {
		return err
	}

	for _, topicMetadata := range topicMetadataResponse.TopicsMetadata {
		partitions := make([]int32, 0)
		for _, partitionMetadata := range topicMetadata.PartitionsMetadata {
			partitions = append(partitions, partitionMetadata.PartitionID)
		}
		tmc.cache[topicMetadata.Topic] = newTopicMetadataCacheEntry(partitions)
	}

	return nil
}

type topicMetadataCacheEntry struct {
	partitions []int32
	timestamp  time.Time
}

func newTopicMetadataCacheEntry(partitions []int32) *topicMetadataCacheEntry {
	return &topicMetadataCacheEntry{
		partitions: partitions,
		timestamp:  time.Now(),
	}
}