package controller

import (
	"context"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func WithManagedConfiguration(enabled bool) Option {
	return func(c *Controller) { c.managedConfig = enabled }
}
func verifiedManagedPeer(ctx context.Context) bool {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return false
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	return ok && len(info.State.VerifiedChains) > 0 && len(info.State.VerifiedChains[0]) > 0
}
func (c *Controller) HasManagedSession(id, session string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.ready[id]
	return s != nil && s.sessionID == session && s.managedConfig && s.sender.ctx.Err() == nil
}
func (c *Controller) SendDesiredConfig(ctx context.Context, id, session string, snapshot *pb.DesiredConfigSnapshot) (bool, error) {
	if snapshot == nil || snapshot.CollectorId != id || snapshot.SessionId != session {
		return false, status.Error(codes.InvalidArgument, "invalid configuration identity")
	}
	c.mu.Lock()
	s := c.ready[id]
	if s == nil || s.sessionID != session || !s.managedConfig || s.sender.ctx.Err() != nil {
		c.mu.Unlock()
		return false, status.Error(codes.FailedPrecondition, "managed configuration was not negotiated on matching session")
	}
	c.mu.Unlock()
	if err := c.authorize(s.sender.ctx, id); err != nil {
		s.sender.stop(err)
		return false, err
	}
	return s.sender.sendContext(ctx, &pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_DesiredConfig{DesiredConfig: proto.Clone(snapshot).(*pb.DesiredConfigSnapshot)}})
}
