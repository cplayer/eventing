/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sender

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"time"

	cloudevents "github.com/cloudevents/sdk-go"
	"github.com/cloudevents/sdk-go/pkg/cloudevents/client"
	"github.com/golang/protobuf/ptypes"
	"github.com/google/uuid"
	"github.com/rogpeppe/fastuuid"
	vegeta "github.com/tsenart/vegeta/lib"
	"knative.dev/eventing/test/common/performance/common"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

type CloudEventsTargeter struct {
	sinkUrl     string
	msgSize     uint
	eventType   string
	eventSource string
}

var letterBytes = []byte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

const markLetter = byte('"')

// generateRandString returns a random string with the given length.
func generateRandStringPayload(length uint) []byte {
	b := make([]byte, length)
	b[0] = markLetter
	for i := uint(1); i < length-1; i++ {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	b[length-1] = markLetter
	return b
}

func NewCloudEventsTargeter(sinkUrl string, msgSize uint, eventType string, eventSource string) CloudEventsTargeter {
	return CloudEventsTargeter{
		sinkUrl:     sinkUrl,
		msgSize:     msgSize,
		eventType:   eventType,
		eventSource: eventSource,
	}
}

func (cet CloudEventsTargeter) VegetaTargeter() vegeta.Targeter {
	uuidGen := fastuuid.MustNewGenerator()

	ceType := []string{cet.eventType}
	ceSource := []string{cet.eventSource}
	ceSpecVersion := []string{"0.2"}
	ceContentType := []string{"application/json"}

	return func(t *vegeta.Target) error {
		t.Method = http.MethodPost
		t.URL = cet.sinkUrl

		t.Header = make(http.Header, 5)

		t.Header["Ce-Id"] = []string{uuidGen.Hex128()}

		t.Header["Ce-Type"] = ceType
		t.Header["Ce-Source"] = ceSource
		t.Header["Ce-Specversion"] = ceSpecVersion
		t.Header["Content-Type"] = ceContentType
		t.Body = generateRandStringPayload(cet.msgSize)

		return nil
	}
}

type HttpLoadGenerator struct {
	eventSource string
	sinkUrl     string

	sentCh     chan common.EventTimestamp
	acceptedCh chan common.EventTimestamp

	warmupAttacker *vegeta.Attacker
	paceAttacker   *vegeta.Attacker
	ceClient       client.Client
}

func NewHttpLoadGeneratorFactory(sinkUrl string, minWorkers uint64) LoadGeneratorFactory {
	return func(eventSource string, sentCh chan common.EventTimestamp, acceptedCh chan common.EventTimestamp) (generator LoadGenerator, e error) {
		if sinkUrl == "" {
			panic("Missing --sink flag")
		}

		loadGen := &HttpLoadGenerator{
			eventSource: eventSource,
			sinkUrl:     sinkUrl,

			sentCh:     sentCh,
			acceptedCh: acceptedCh,
		}

		loadGen.warmupAttacker = vegeta.NewAttacker(vegeta.Workers(minWorkers))
		loadGen.paceAttacker = vegeta.NewAttacker(
			vegeta.Client(&http.Client{Transport: requestInterceptor{
				before: func(request *http.Request) {
					id := request.Header.Get("Ce-Id")
					loadGen.sentCh <- common.EventTimestamp{EventId: id, At: ptypes.TimestampNow()}
				},
				after: func(request *http.Request, response *http.Response, e error) {
					id := request.Header.Get("Ce-Id")
					t := ptypes.TimestampNow()
					if e == nil && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
						loadGen.acceptedCh <- common.EventTimestamp{EventId: id, At: t}
					}
				},
			}}),
			vegeta.Workers(minWorkers),
			vegeta.MaxBody(0),
		)

		var err error
		loadGen.ceClient, err = newCloudEventsClient(sinkUrl)
		if err != nil {
			return nil, err
		}

		return loadGen, nil
	}
}

func newCloudEventsClient(sinkUrl string) (client.Client, error) {
	t, err := cloudevents.NewHTTPTransport(
		cloudevents.WithBinaryEncoding(),
		cloudevents.WithTarget(sinkUrl),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create transport: %v", err)
	}

	return cloudevents.NewClient(t)
}

func (h HttpLoadGenerator) Warmup(pace common.PaceSpec, msgSize uint) {
	targeter := NewCloudEventsTargeter(h.sinkUrl, msgSize, common.WarmupEventType, defaultEventSource).VegetaTargeter()
	vegetaResults := h.warmupAttacker.Attack(targeter, vegeta.ConstantPacer{Freq: pace.Rps, Per: time.Second}, pace.Duration, common.WarmupEventType+"-attack")
	for range vegetaResults {
	}
}

func (h HttpLoadGenerator) RunPace(i int, pace common.PaceSpec, msgSize uint) {
	targeter := NewCloudEventsTargeter(h.sinkUrl, msgSize, common.MeasureEventType, eventsSource()).VegetaTargeter()
	res := h.paceAttacker.Attack(targeter, vegeta.ConstantPacer{Freq: pace.Rps, Per: time.Second}, pace.Duration, fmt.Sprintf("%s-attack-%d", h.eventSource, i))
	for range res {
	}
}

func (h HttpLoadGenerator) SendGCEvent() {
	event := cloudevents.NewEvent(cloudevents.VersionV02)
	event.SetID(uuid.New().String())
	event.SetType(common.GCEventType)
	event.SetSource(h.eventSource)

	_, _, _ = h.ceClient.Send(context.TODO(), event)
}

func (h HttpLoadGenerator) SendEndEvent() {
	event := cloudevents.NewEvent(cloudevents.VersionV02)
	event.SetID(uuid.New().String())
	event.SetType(common.EndEventType)
	event.SetSource(h.eventSource)

	_, _, _ = h.ceClient.Send(context.TODO(), event)
}
