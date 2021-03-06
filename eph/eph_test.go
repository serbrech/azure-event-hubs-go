package eph

//	MIT License
//
//	Copyright (c) Microsoft Corporation. All rights reserved.
//
//	Permission is hereby granted, free of charge, to any person obtaining a copy
//	of this software and associated documentation files (the "Software"), to deal
//	in the Software without restriction, including without limitation the rights
//	to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
//	copies of the Software, and to permit persons to whom the Software is
//	furnished to do so, subject to the following conditions:
//
//	The above copyright notice and this permission notice shall be included in all
//	copies or substantial portions of the Software.
//
//	THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
//	IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
//	FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
//	AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
//	LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
//	OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
//	SOFTWARE

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-amqp-common-go/aad"
	"github.com/Azure/azure-amqp-common-go/auth"
	"github.com/Azure/azure-event-hubs-go"
	"github.com/Azure/azure-event-hubs-go/internal/test"
	mgmt "github.com/Azure/azure-sdk-for-go/services/eventhub/mgmt/2017-04-01/eventhub"
	"github.com/stretchr/testify/suite"
)

type (
	// eventHubSuite encapsulates a end to end test of Event Hubs with build up and tear down of all EH resources
	testSuite struct {
		test.BaseSuite
	}
)

func TestEventProcessorHost(t *testing.T) {
	suite.Run(t, new(testSuite))
}

func (s *testSuite) TestSingle() {
	hub, del := s.ensureRandomHub("goEPH", 10)
	defer del()

	processor, err := s.newInMemoryEPH(*hub.Name)
	if err != nil {
		s.T().Fatal(err)
	}

	messages, err := s.sendMessages(*hub.Name, 10)
	if err != nil {
		s.T().Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(len(messages))

	processor.Receive(func(c context.Context, event *eventhub.Event) error {
		wg.Done()
		return nil
	})

	processor.StartNonBlocking(context.Background())
	defer func() {
		closeContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		processor.Close(closeContext)
		cancel()
	}()

	waitUntil(s.T(), &wg, 30*time.Second)
}

func (s *testSuite) TestMultiple() {
	hub, del := s.ensureRandomHub("goEPH", 10)
	numPartitions := len(*hub.PartitionIds)
	sharedStore := new(sharedStore)
	processors := make(map[string]*EventProcessorHost, numPartitions)
	processorNames := make([]string, numPartitions)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < numPartitions; i++ {
		processor, err := s.newInMemoryEPHWithOptions(*hub.Name, sharedStore)
		if err != nil {
			s.T().Fatal(err)
		}
		processors[processor.GetName()] = processor
		processor.StartNonBlocking(ctx)
		processorNames[i] = processor.GetName()
	}

	defer func() {
		for _, processor := range processors {
			closeContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			processor.Close(closeContext)
			cancel()
		}
		del()
	}()

	count := 0
	var partitionsByProcessor map[string][]int
	balanced := false
	for {
		<-time.After(3 * time.Second)
		count++
		if count > 50 {
			break
		}

		partitionsByProcessor = make(map[string][]int, len(*hub.PartitionIds))
		for _, processor := range processors {
			partitions := processor.PartitionIDsBeingProcessed()
			partitionInts, err := stringsToInts(partitions)
			if err != nil {
				s.T().Fatal(err)
			}
			partitionsByProcessor[processor.GetName()] = partitionInts
		}

		if allHaveOnePartition(partitionsByProcessor, numPartitions) {
			balanced = true
			break
		}
	}
	if !balanced {
		s.T().Error("never balanced work within allotted time")
		return
	}

	closeContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	processors[processorNames[numPartitions-1]].Close(closeContext) // close the last partition
	delete(processors, processorNames[numPartitions-1])
	cancel()

	count = 0
	balanced = false
	for {
		<-time.After(3 * time.Second)
		count++
		if count > 50 {
			break
		}

		partitionsByProcessor = make(map[string][]int, len(*hub.PartitionIds))
		for _, processor := range processors {
			partitions := processor.PartitionIDsBeingProcessed()
			partitionInts, err := stringsToInts(partitions)
			if err != nil {
				s.T().Fatal(err)
			}
			partitionsByProcessor[processor.GetName()] = partitionInts
		}

		if allHandled(partitionsByProcessor, len(*hub.PartitionIds)) {
			balanced = true
			break
		}
	}
	if !balanced {
		s.T().Error("didn't balance after closing a processor")
	}
}

func (s *testSuite) sendMessages(hubName string, length int) ([]string, error) {
	client := s.newClient(s.T(), hubName)
	defer func() {
		closeContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		client.Close(closeContext)
		cancel()
	}()

	messages := make([]string, length)
	for i := 0; i < length; i++ {
		messages[i] = s.RandomName("message", 5)
	}

	events := make([]*eventhub.Event, length)
	for idx, msg := range messages {
		events[idx] = eventhub.NewEventFromString(msg)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client.SendBatch(ctx, eventhub.NewEventBatch(events))

	return messages, ctx.Err()
}

func (s *testSuite) ensureRandomHub(prefix string, length int) (*mgmt.Model, func()) {
	hubName := s.RandomName(prefix, length)
	hub, err := s.EnsureEventHub(context.Background(), hubName)
	if err != nil {
		s.T().Fatal(err)
	}

	return hub, func() {
		s.DeleteEventHub(context.Background(), hubName)
	}
}

func (s *testSuite) newInMemoryEPH(hubName string) (*EventProcessorHost, error) {
	return s.newInMemoryEPHWithOptions(hubName, new(sharedStore))
}

func (s *testSuite) newInMemoryEPHWithOptions(hubName string, store *sharedStore) (*EventProcessorHost, error) {
	provider, err := aad.NewJWTProvider(aad.JWTProviderWithEnvironmentVars())
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	leaserCheckpointer := newMemoryLeaserCheckpointer(DefaultLeaseDuration, store)
	processor, err := New(ctx, s.Namespace, hubName, provider, leaserCheckpointer, leaserCheckpointer, WithNoBanner())
	if err != nil {
		return nil, err
	}

	return processor, nil
}

func (s *testSuite) newClient(t *testing.T, hubName string, opts ...eventhub.HubOption) *eventhub.Hub {
	provider, err := aad.NewJWTProvider(aad.JWTProviderWithEnvironmentVars(), aad.JWTProviderWithAzureEnvironment(&s.Env))
	if err != nil {
		t.Fatal(err)
	}
	return s.newClientWithProvider(t, hubName, provider, opts...)
}

func (s *testSuite) newClientWithProvider(t *testing.T, hubName string, provider auth.TokenProvider, opts ...eventhub.HubOption) *eventhub.Hub {
	opts = append(opts, eventhub.HubWithEnvironment(s.Env))
	client, err := eventhub.NewHub(s.Namespace, hubName, provider, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func waitUntil(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return
	case <-time.After(d):
		t.Error("took longer than " + fmtDuration(d))
	}
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second) / time.Second
	return fmt.Sprintf("%d seconds", d)
}

func allHaveOnePartition(partitionsByProcessor map[string][]int, numberOfPartitions int) bool {
	for _, partitions := range partitionsByProcessor {
		if len(partitions) != 1 {
			return false
		}
	}

	countByPartition := make(map[int]int, numberOfPartitions)
	for i := 0; i < numberOfPartitions; i++ {
		countByPartition[i] = 0
	}
	for _, partitions := range partitionsByProcessor {
		for _, partition := range partitions {
			if count, ok := countByPartition[partition]; ok {
				countByPartition[partition] = count + 1
			}
		}
	}
	for i := 0; i < numberOfPartitions; i++ {
		if countByPartition[i] != 1 {
			return false
		}
	}
	return true
}

func allHandled(partitionsByProcessor map[string][]int, numberOfPartitions int) bool {
	countByPartition := make(map[int]int, numberOfPartitions)
	for i := 0; i < numberOfPartitions; i++ {
		countByPartition[i] = 0
	}
	for _, partitions := range partitionsByProcessor {
		for _, partition := range partitions {
			if count, ok := countByPartition[partition]; ok {
				countByPartition[partition] = count + 1
			}
		}
	}

	for _, count := range countByPartition {
		if count != 1 {
			return false
		}
	}
	return true
}

func stringsToInts(strs []string) ([]int, error) {
	ints := make([]int, len(strs))
	for idx, str := range strs {
		strInt, err := strconv.Atoi(str)
		if err != nil {
			return nil, err
		}
		ints[idx] = strInt
	}
	return ints, nil
}
