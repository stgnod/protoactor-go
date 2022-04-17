package remote

import (
	"errors"
	"io"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/asynkron/protoactor-go/log"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func endpointWriterProducer(remote *Remote, address string, config *Config) actor.Producer {
	return func() actor.Actor {
		return &endpointWriter{
			address: address,
			config:  config,
			remote:  remote,
		}
	}
}

type endpointWriter struct {
	config  *Config
	address string
	conn    *grpc.ClientConn
	stream  Remoting_ReceiveClient
	remote  *Remote
}

func (state *endpointWriter) initialize() {
	now := time.Now()
	plog.Info("Started EndpointWriter. connecting", log.String("address", state.address))
	err := state.initializeInternal()
	if err != nil {
		plog.Error("EndpointWriter failed to connect", log.String("address", state.address), log.Error(err))
		// Wait 2 seconds to restart and retry
		// Replace with Exponential Backoff
		time.Sleep(2 * time.Second)
		panic(err)
	}
	plog.Info("EndpointWriter connected", log.String("address", state.address), log.Duration("cost", time.Since(now)))
}

func (state *endpointWriter) initializeInternal() error {
	conn, err := grpc.Dial(state.address, state.config.DialOptions...)
	if err != nil {
		return err
	}
	state.conn = conn
	c := NewRemotingClient(conn)
	stream, err := c.Receive(context.Background(), state.config.CallOptions...)
	if err != nil {
		plog.Error("EndpointWriter failed to create receive stream", log.String("address", state.address), log.Error(err))
		return err
	}
	state.stream = stream

	err = stream.Send(&RemoteMessage{
		MessageType: &RemoteMessage_ConnectRequest{
			ConnectRequest: &ConnectRequest{
				ConnectionType: &ConnectRequest_ServerConnection{
					ServerConnection: &ServerConnection{
						SystemId: state.remote.actorSystem.ID,
						Address:  state.remote.actorSystem.Address(),
					},
				},
			},
		},
	})
	if err != nil {
		plog.Error("EndpointWriter failed to send connect request", log.String("address", state.address), log.Error(err))
		return err
	}

	connection, err := stream.Recv()
	if err != nil {
		plog.Error("EndpointWriter failed to send connect request", log.String("address", state.address), log.Error(err))
		return err
	}

	switch connection.MessageType.(type) {
	case *RemoteMessage_ConnectResponse:
		break
	default:
		plog.Error("EndpointWriter failed to receive connect response", log.String("address", state.address), log.TypeOf("type", connection.MessageType))
		return errors.New("invalid connect response")
	}

	go func() {
		for {
			_, err := stream.Recv()
			switch {
			case errors.Is(err, io.EOF):
				plog.Debug("EndpointWriter stream completed", log.String("address", state.address))
				break
			case err != nil:
				plog.Error("EndpointWriter lost connection", log.String("address", state.address), log.Error(err))
				terminated := &EndpointTerminatedEvent{
					Address: state.address,
				}
				state.remote.actorSystem.EventStream.Publish(terminated)
				return
			default:
				plog.Info("EndpointWriter remote disconnected", log.String("address", state.address))
				terminated := &EndpointTerminatedEvent{
					Address: state.address,
				}
				state.remote.actorSystem.EventStream.Publish(terminated)
			}
		}
	}()

	connected := &EndpointConnectedEvent{Address: state.address}
	state.remote.actorSystem.EventStream.Publish(connected)
	state.stream = stream
	return nil
}

func (state *endpointWriter) sendEnvelopes(msg []interface{}, ctx actor.Context) {
	envelopes := make([]*MessageEnvelope, len(msg))

	// type name uniqueness map name string to type index
	typeNames := make(map[string]int32)
	typeNamesArr := make([]string, 0)

	targetNames := make(map[string]int32)
	targetNamesArr := make([]*actor.PID, 0)

	senderNames := make(map[string]int32)
	senderNamesArr := make([]*actor.PID, 0)

	var (
		header       *MessageHeader
		typeID       int32
		targetID     int32
		senderID     int32
		serializerID int32
	)

	for i, tmp := range msg {
		switch unwrapped := tmp.(type) {
		case *EndpointTerminatedEvent, EndpointTerminatedEvent:
			plog.Debug("Handling array wrapped terminate event", log.String("address", state.address), log.Object("msg", unwrapped))
			ctx.Stop(ctx.Self())
			return
		}

		rd, _ := tmp.(*remoteDeliver)

		if rd.header == nil || rd.header.Length() == 0 {
			header = nil
		} else {
			header = &MessageHeader{
				HeaderData: rd.header.ToMap(),
			}
		}

		bytes, typeName, err := Serialize(rd.message, serializerID)
		if err != nil {
			panic(err)
		}
		typeID, typeNamesArr = addToLookup(typeNames, typeName, typeNamesArr)
		targetID, targetNamesArr = addToPidLookup(targetNames, rd.target, targetNamesArr)
		senderID, senderNamesArr = addToPidLookup(senderNames, rd.sender, senderNamesArr)

		targetRequestID := uint32(0)
		if rd.target != nil {
			targetRequestID = rd.target.RequestId
		}

		senderRequestID := uint32(0)
		if rd.sender != nil {
			senderRequestID = rd.sender.RequestId
		}

		envelopes[i] = &MessageEnvelope{
			MessageHeader:   header,
			MessageData:     bytes,
			Sender:          senderID,
			Target:          targetID,
			TypeId:          typeID,
			SerializerId:    serializerID,
			TargetRequestId: targetRequestID,
			SenderRequestId: senderRequestID,
		}
	}

	err := state.stream.Send(&RemoteMessage{
		MessageType: &RemoteMessage_MessageBatch{
			MessageBatch: &MessageBatch{
				TypeNames: typeNamesArr,
				Targets:   targetNamesArr,
				Senders:   senderNamesArr,
				Envelopes: envelopes,
			},
		},
	})
	if err != nil {
		ctx.Stash()
		plog.Debug("gRPC Failed to send", log.String("address", state.address), log.Error(err))
		panic("restart it")
	}
}

func addToLookup(m map[string]int32, name string, a []string) (int32, []string) {
	max := int32(len(m))
	id, ok := m[name]
	if !ok {
		m[name] = max
		id = max
		a = append(a, name)
	}
	return id, a
}

func addToPidLookup(m map[string]int32, pid *actor.PID, arr []*actor.PID) (int32, []*actor.PID) {
	if pid == nil {
		return 0, arr
	}

	max := int32(len(m))
	key := pid.Address + "/" + pid.Id
	id, ok := m[key]
	if !ok {
		c, _ := proto.Clone(pid).(*actor.PID)
		c.RequestId = 0
		m[key] = max
		id = max
		arr = append(arr, c)
	}
	return id + 1, arr
}

func (state *endpointWriter) Receive(ctx actor.Context) {
	switch msg := ctx.Message().(type) {
	case *actor.Started:
		state.initialize()
	case *actor.Stopped:
		state.closeClientConn()
	case *actor.Restarting:
		state.closeClientConn()
	case *EndpointTerminatedEvent:
		plog.Info("Stopping EnpointWriter", log.String("address", state.address))
		ctx.Stop(ctx.Self())
	case []interface{}:
		state.sendEnvelopes(msg, ctx)
	case actor.SystemMessage, actor.AutoReceiveMessage:
		// ignore
	default:
		plog.Error("EndpointWriter received unknown message", log.String("address", state.address), log.TypeOf("type", msg), log.Message(msg))
	}
}

func (state *endpointWriter) closeClientConn() {
	if state.stream != nil {
		err := state.stream.CloseSend()
		if err != nil {
			plog.Error("EndpointWriter error when closing the stream", log.Error(err))
		}
	}
	if state.conn != nil {
		err := state.conn.Close()
		if err != nil {
			plog.Error("EndpointWriter error when closing the client conn", log.Error(err))
		}
	}
}
