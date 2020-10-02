package rpc

import (
	"context"
	"io"
	"log"
	"time"

	"github.com/hashicorp/packer/common/retry"
	"github.com/hashicorp/packer/packer"
)

// An implementation of packer.PostProcessor where the PostProcessor is actually
// executed over an RPC connection.
type postProcessor struct {
	commonClient
}

// PostProcessorServer wraps a packer.PostProcessor implementation and makes it
// exportable as part of a Golang RPC server.
type PostProcessorServer struct {
	context       context.Context
	contextCancel func()

	commonServer
	p packer.PostProcessor
}

type PostProcessorConfigureArgs struct {
	Configs []interface{}
}

type PostProcessorProcessResponse struct {
	Err           *BasicError
	Keep          bool
	ForceOverride bool
	StreamId      uint32
}

func (p *postProcessor) Configure(raw ...interface{}) error {
	raw, err := encodeCTYValues(raw)
	if err != nil {
		return err
	}
	args := &PostProcessorConfigureArgs{Configs: raw}
	return p.client.Call(p.endpoint+".Configure", args, new(interface{}))
}

func (p *postProcessor) PostProcess(ctx context.Context, ui packer.Ui, a packer.Artifact) (packer.Artifact, bool, bool, error) {
	nextId := p.mux.NextId()
	server := newServerWithMux(p.mux, nextId)
	server.RegisterArtifact(a)
	server.RegisterUi(ui)
	go server.Serve()

	done := make(chan interface{})
	defer close(done)

	go func() {
		select {
		case <-ctx.Done():
			log.Printf("Cancelling post-processor after context cancellation %v", ctx.Err())
			if err := p.client.Call(p.endpoint+".Cancel", new(interface{}), new(interface{})); err != nil {
				log.Printf("Error cancelling post-processor: %s", err)
			}
		case <-done:
		}
	}()

	var response PostProcessorProcessResponse
	if err := p.client.Call(p.endpoint+".PostProcess", nextId, &response); err != nil {
		return nil, false, false, err
	}

	if response.Err != nil {
		return nil, false, false, response.Err
	}

	if response.StreamId == 0 {
		return nil, false, false, nil
	}

	if response.StreamId == nextId {
		log.Printf("Returned the same artifact")
		return a, response.Keep, response.ForceOverride, nil
	}

	var client *Client
	err := retry.Config{
		RetryDelay: (&retry.Backoff{InitialBackoff: time.Second / 2}).Linear,
		ShouldRetry: func(err error) bool {
			if err == io.EOF {
				return true
			}
			return false
		},
	}.Run(context.TODO(), func(_ context.Context) (err error) {
		log.Printf("connecting to artifact %v", response.StreamId)
		client, err = newClientWithMux(p.mux, response.StreamId)
		if err != nil {
			log.Printf("failed to start client: %v", err)
		}
		return err
	})
	if err != nil {
		log.Printf("failed to start client: %v", err)
		return nil, false, false, err
	}

	return client.Artifact(), response.Keep, response.ForceOverride, nil
}

func (p *PostProcessorServer) Configure(args *PostProcessorConfigureArgs, reply *interface{}) (err error) {
	config, err := decodeCTYValues(args.Configs)
	if err != nil {
		return err
	}
	err = p.p.Configure(config...)
	return err
}

func (p *PostProcessorServer) PostProcess(streamId uint32, reply *PostProcessorProcessResponse) (err error) {
	defer func() {
		log.Printf("returning %v && %v", *reply, err)
	}()
	client, err := newClientWithMux(p.mux, streamId)
	if err != nil {
		return NewBasicError(err)
	}

	if p.context == nil {
		p.context, p.contextCancel = context.WithCancel(context.Background())
	}

	artifact := client.Artifact()
	artifactResult, keep, forceOverride, err := p.p.PostProcess(p.context, client.Ui(), artifact)
	*reply = PostProcessorProcessResponse{
		Err:           NewBasicError(err),
		Keep:          keep,
		ForceOverride: forceOverride,
		StreamId:      0,
	}
	if err != nil {
		log.Printf("error: %v", err)
		client.Close()
		return nil
	}

	if artifactResult == artifact {
		reply.StreamId = streamId
		log.Printf("same: %v", streamId)
		log.Printf("same: %v", reply.StreamId)
		// client.Close()
		return nil
	}

	if artifactResult != nil {
		streamId = p.mux.NextId()
		reply.StreamId = streamId
		server := newServerWithMux(p.mux, streamId)
		server.RegisterArtifact(artifactResult)
		go server.Serve()

		// client.Close()
		log.Printf("new")
	}

	return nil
}

func (b *PostProcessorServer) Cancel(args *interface{}, reply *interface{}) error {
	if b.contextCancel != nil {
		b.contextCancel()
	}
	return nil
}
