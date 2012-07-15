package main

import (
	"../nsq"
	"../util/pqueue"
	"bitly/notify"
	"container/heap"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// the amount of time a worker will wait when idle
const defaultWorkerWait = 250 * time.Millisecond

// Channel represents the concrete type for a NSQ channel (and also
// implements the Queue interface)
//
// There can be many channels per topic and each with there own distinct 
// clients subscribed.
//
// Channels maintain all client and message, orchestrating in-flight
// messages, timeouts, requeueing, etc.
type Channel struct {
	sync.RWMutex // embed a r/w mutex

	topicName  string
	name       string
	msgTimeout int64

	backend nsq.BackendQueue

	incomingMessageChan chan *nsq.Message
	memoryMsgChan       chan *nsq.Message
	clientMessageChan   chan *nsq.Message
	exitChan            chan int

	// state tracking
	clients          []*nsq.ServerClient
	requeuePQ        pqueue.PriorityQueue
	requeueMutex     sync.Mutex
	inFlightMessages map[string]interface{}
	inFlightPQ       pqueue.PriorityQueue
	inFlightMutex    sync.Mutex

	// stat counters
	requeueCount uint64
	getCount     uint64
	putCount     uint64
	timeoutCount uint64
}

type inFlightMessage struct {
	msg    *nsq.Message
	client *nsq.ServerClient
}

// NewChannel creates a new instance of the Channel type and returns a pointer
func NewChannel(topicName string, channelName string, inMemSize int64, dataPath string, maxBytesPerFile int64, msgTimeout int64) *Channel {
	c := &Channel{
		topicName:  topicName,
		name:       channelName,
		msgTimeout: msgTimeout,
		// backend names, for uniqueness, automatically include the topic... <topic>:<channel>
		backend:             NewDiskQueue(topicName+":"+channelName, dataPath, maxBytesPerFile),
		incomingMessageChan: make(chan *nsq.Message, 5),
		memoryMsgChan:       make(chan *nsq.Message, inMemSize),
		clientMessageChan:   make(chan *nsq.Message),
		exitChan:            make(chan int),
		clients:             make([]*nsq.ServerClient, 0, 5),
		inFlightMessages:    make(map[string]interface{}),
		inFlightPQ:          pqueue.New(int(inMemSize / 10)),
		requeuePQ:           pqueue.New(int(inMemSize / 10)),
	}
	go c.router()
	go c.messagePump()
	go c.requeueWorker()
	go c.inFlightWorker()
	notify.Post("new_channel", c)
	return c
}

// Close cleanly closes the Channel
func (c *Channel) Close() error {
	var err error

	log.Printf("CHANNEL(%s): closing", c.name)

	close(c.exitChan)
	FlushQueue(c)

	err = c.backend.Close()
	if err != nil {
		return err
	}

	return nil
}

// MemoryChan implements the Queue interface
func (c *Channel) MemoryChan() chan *nsq.Message {
	return c.memoryMsgChan
}

// BackendQueue implements the Queue interface
func (c *Channel) BackendQueue() nsq.BackendQueue {
	return c.backend
}

// InFlight implements the Queue interface
func (c *Channel) InFlight() map[string]interface{} {
	return c.inFlightMessages
}

// PutMessage writes to the appropriate incoming message channel
// (which will be routed asynchronously)
func (c *Channel) PutMessage(msg *nsq.Message) {
	c.incomingMessageChan <- msg
	atomic.AddUint64(&c.putCount, 1)
}

// FinishMessage successfully discards an in-flight message
func (c *Channel) FinishMessage(id []byte) error {
	item, err := c.popInFlightMessage(id)
	if err != nil {
		log.Printf("ERROR: failed to finish message(%s) - %s", id, err.Error())
	} else {
		c.removeFromInFlightPQ(item)
	}
	return err
}

// RequeueMessage requeues a message based on `time.Duration`, ie:
//
// `timeoutMs` == 0 - requeue a message immediately
// `timeoutMs`  > 0 - asynchronously wait for the specified timeout
//     and requeue a message (aka "deferred requeue")
//
func (c *Channel) RequeueMessage(id []byte, timeout time.Duration) error {
	if timeout == 0 {
		return c.doRequeue(id)
	}

	item, err := c.getInFlightMessage(id)
	if err != nil {
		return err
	}
	// TODO: fix bug where two clients simultaneously REQ the same id
	// cause this remove to crash (it assumes that the item is on the queue)
	// a possible better long term solution is to resolve the issue
	// of overloading the in-flight data structures with deferred requeue 
	// messages, we would not have to keep the item on the in-flight map
	c.removeFromInFlightPQ(item)
	c.addToDeferredPQ(item.Value.(*inFlightMessage).msg, timeout)

	return nil
}

// AddClient adds the ServerClient the Channel's client list
func (c *Channel) AddClient(client *nsq.ServerClient) {
	c.Lock()
	defer c.Unlock()

	found := false
	for _, cli := range c.clients {
		if cli == client {
			found = true
			break
		}
	}

	if !found {
		c.clients = append(c.clients, client)
	}
}

// RemoveClient removes the ServerClient from the Channel's client list
func (c *Channel) RemoveClient(client *nsq.ServerClient) {
	c.Lock()
	defer c.Unlock()

	if len(c.clients) == 0 {
		return
	}

	finalClients := make([]*nsq.ServerClient, 0, len(c.clients)-1)
	for _, cli := range c.clients {
		if cli != client {
			finalClients = append(finalClients, cli)
		}
	}

	c.clients = finalClients
}

func (c *Channel) StartInflightTimeout(msg *nsq.Message, client *nsq.ServerClient) error {
	value := &inFlightMessage{msg, client}
	absTs := time.Now().UnixNano() + c.msgTimeout
	item := &pqueue.Item{Value: value, Priority: -absTs}
	err := c.pushInFlightMessage(item)
	if err != nil {
		return err
	}
	c.addToInFlightPQ(item)
	return nil
}

// doRequeue performs the low level operations to requeue a message
func (c *Channel) doRequeue(id []byte) error {
	item, err := c.popInFlightMessage(id)
	if err != nil {
		log.Printf("ERROR: failed to requeue message(%s) - %s", id, err.Error())
	} else {
		atomic.AddUint64(&c.requeueCount, 1)
		msg := item.Value.(*inFlightMessage).msg
		c.incomingMessageChan <- msg
	}
	return err
}

// pushInFlightMessage atomically adds a message to the in-flight dictionary
func (c *Channel) pushInFlightMessage(item *pqueue.Item) error {
	c.Lock()
	defer c.Unlock()

	id := item.Value.(*inFlightMessage).msg.Id
	_, ok := c.inFlightMessages[string(id)]
	if ok {
		return errors.New("E_ID_ALREADY_IN_FLIGHT")
	}
	c.inFlightMessages[string(id)] = item

	return nil
}

// popInFlightMessage atomically removes a message from the in-flight dictionary
func (c *Channel) popInFlightMessage(id []byte) (*pqueue.Item, error) {
	c.Lock()
	defer c.Unlock()

	item, ok := c.inFlightMessages[string(id)]
	if !ok {
		return nil, errors.New("E_ID_NOT_IN_FLIGHT")
	}
	delete(c.inFlightMessages, string(id))

	return item.(*pqueue.Item), nil
}

// getInFlightMessage retrieves a message from the in-flight dictionary by ID
func (c *Channel) getInFlightMessage(id []byte) (*pqueue.Item, error) {
	c.RLock()
	defer c.RUnlock()

	item, ok := c.inFlightMessages[string(id)]
	if !ok {
		return nil, errors.New("E_ID_NOT_IN_FLIGHT")
	}

	return item.(*pqueue.Item), nil
}

func (c *Channel) addToInFlightPQ(item *pqueue.Item) {
	c.inFlightMutex.Lock()
	defer c.inFlightMutex.Unlock()
	heap.Push(&c.inFlightPQ, item)
}

func (c *Channel) removeFromInFlightPQ(item *pqueue.Item) {
	c.inFlightMutex.Lock()
	defer c.inFlightMutex.Unlock()
	heap.Remove(&c.inFlightPQ, item.Index)
}

func (c *Channel) addToDeferredPQ(msg *nsq.Message, timeout time.Duration) {
	c.requeueMutex.Lock()
	defer c.requeueMutex.Unlock()
	absTs := time.Now().UnixNano() + int64(timeout)
	heap.Push(&c.requeuePQ, &pqueue.Item{Value: msg, Priority: -absTs})
}

// Router handles the muxing of incoming Channel messages, either writing
// to the in-memory channel or to the backend
func (c *Channel) router() {
	for {
		select {
		case msg := <-c.incomingMessageChan:
			select {
			case c.memoryMsgChan <- msg:
			default:
				err := WriteMessageToBackend(msg, c)
				if err != nil {
					log.Printf("ERROR: failed to write message to backend - %s", err.Error())
					// TODO: requeue?
					continue
				}
			}
		case <-c.exitChan:
			return
		}
	}
}

// messagePump reads messages from either memory or backend and writes
// to the client output go channel
//
// it is also performs in-flight accounting and initiates the auto-requeue
// goroutine
func (c *Channel) messagePump() {
	var msg *nsq.Message
	var buf []byte
	var err error

	for {
		select {
		case msg = <-c.memoryMsgChan:
		case buf = <-c.backend.ReadChan():
			msg, err = nsq.DecodeMessage(buf)
			if err != nil {
				log.Printf("ERROR: failed to decode message - %s", err.Error())
				continue
			}
		case <-c.exitChan:
			return
		}

		msg.Attempts++

		c.clientMessageChan <- msg
		// the client will call back to mark as in-flight w/ it's info

		atomic.AddUint64(&c.getCount, 1)
	}
}

func (c *Channel) requeueWorker() {
	pqWorker(&c.requeuePQ, &c.requeueMutex, func(item *pqueue.Item) {
		msg := item.Value.(*nsq.Message)
		c.doRequeue(msg.Id)
	})
}

func (c *Channel) inFlightWorker() {
	pqWorker(&c.inFlightPQ, &c.inFlightMutex, func(item *pqueue.Item) {
		value := item.Value.(*inFlightMessage)
		atomic.AddUint64(&c.timeoutCount, 1)
		value.client.TimedOutMessage()
		c.doRequeue(value.msg.Id)
	})
}

// generic loop (executed in a goroutine) that periodically wakes up to walk
// the specified (chronological) priority queue and call the callback
//
// if the first element on the queue is not ready (not enough time has elapsed)
// the amount of time to wait before the next iteration is adjusted to optimize
//
// TODO: fix edge case where you're waiting and a new element is concurrently
// added that has a lower timeout (ie. added as the first element)
func pqWorker(pq *pqueue.PriorityQueue, mutex *sync.Mutex, callback func(item *pqueue.Item)) {
	waitTime := defaultWorkerWait
	for {
		<-time.After(waitTime)
		now := time.Now().UnixNano()
		for {
			mutex.Lock()
			if pq.Len() == 0 {
				mutex.Unlock()
				waitTime = defaultWorkerWait
				break
			}
			item := pq.Peek().(*pqueue.Item)
			// priorities are stored negative so that Pop() will return the lowest
			if now < -item.Priority {
				waitTime = time.Duration((-item.Priority)-now) + time.Millisecond
				mutex.Unlock()
				break
			}
			item = heap.Pop(pq).(*pqueue.Item)
			mutex.Unlock()

			callback(item)
		}
	}
}
