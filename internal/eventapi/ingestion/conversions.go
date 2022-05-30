package ingestion

import (
	"context"
	"time"

	"github.com/gogo/protobuf/proto"
	log "github.com/sirupsen/logrus"

	"github.com/G-Research/armada/internal/common/compress"
	"github.com/G-Research/armada/internal/common/eventutil"
	"github.com/G-Research/armada/internal/eventapi/model"
	"github.com/G-Research/armada/internal/pulsarutils"
	"github.com/G-Research/armada/pkg/armadaevents"
)

// MessageRowConverter raw converts pulsar messages into events that we can insert into the database
type MessageRowConverter struct {
	compressor compress.Compressor
}

// Convert takes a  channel of pulsar message batches and outputs a channel of batched events that we can insert into the database
func Convert(ctx context.Context, msgs chan []*pulsarutils.ConsumerMessage, bufferSize int, converter *MessageRowConverter) chan *model.BatchUpdate {
	out := make(chan *model.BatchUpdate, bufferSize)
	go func() {
		for pulsarBatch := range msgs {
			out <- converter.ConvertBatch(ctx, pulsarBatch)
		}
		close(out)
	}()
	return out
}

func (rc *MessageRowConverter) ConvertBatch(ctx context.Context, batch []*pulsarutils.ConsumerMessage) *model.BatchUpdate {

	// First unmarshall everything
	messageIds := make([]*pulsarutils.ConsumerMessageId, len(batch))
	events := make([]*model.Event, 0, len(batch))

	for i, msg := range batch {

		pulsarMsg := msg.Message

		// Record the messageId- we need to record all message Ids, even if the event they contain is invalid
		// As they must be acked at the end
		messageIds[i] = &pulsarutils.ConsumerMessageId{MessageId: pulsarMsg.ID(), ConsumerId: msg.ConsumerId}

		// If it's not a control message then ignore
		if !armadaevents.IsControlMessage(pulsarMsg) {
			continue
		}

		//  If there's no index on the message ignore
		if pulsarMsg.Index() == nil {
			log.Warnf("Index not found on pulsar message %s. Ignoring", pulsarMsg.ID())
			continue
		}

		// Try and unmarshall the proto
		es, err := eventutil.UnmarshalEventSequence(ctx, msg.Message.Payload())
		if err != nil {
			log.WithError(err).Warnf("Could not unmarshal proto for msg %s", pulsarMsg.ID())
			continue
		}

		// Fill in the created time if it's missing
		// TODO: we can remove this once created is being populated everywhere
		for _, event := range es.Events {
			if event.Created == nil {
				t := msg.Message.PublishTime().In(time.UTC)
				event.Created = &t
			}
		}

		// Remove the jobset Name and the queue from the proto as this wil be stored as the key in the db
		queue := es.Queue
		jobset := es.JobSetName
		es.JobSetName = ""
		es.Queue = ""

		bytes, err := proto.Marshal(&armadaevents.DatabaseSequence{EventSequence: es})
		if err != nil {
			log.WithError(err).Warnf("Could not compress proto for msg %s", batch[i].Message.ID())
		}
		compressedBytes, err := rc.compressor.Compress(bytes)
		if err != nil {
			log.WithError(err).Warnf("Could not compress proto for msg %s", batch[i].Message.ID())
		}

		events = append(events, &model.Event{
			Queue:  queue,
			Jobset: jobset,
			Event:  compressedBytes,
		})
	}

	return &model.BatchUpdate{
		MessageIds: messageIds,
		Events:     events,
	}

}
