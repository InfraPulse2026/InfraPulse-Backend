package commands

import (
	"context"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"log/slog"
	"time"
)

type DispatchRepository interface {
	QueuedCommands(context.Context) ([]string, error)
	ClaimCommand(context.Context, string) (Record, *pb.Command, error)
	FinishCommandSend(context.Context, string, bool, error) error
	ExpireCommands(context.Context) error
}
type Sender interface {
	SendCommand(context.Context, string, string, *pb.Command) (bool, error)
}

func Run(ctx context.Context, repo DispatchRepository, sender Sender, log *slog.Logger) {
	timer := time.NewTicker(250 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := repo.ExpireCommands(ctx); err != nil {
			if ctx.Err() == nil {
				log.Error("cannot evaluate command deadlines", "error", err)
			}
			continue
		}
		ids, err := repo.QueuedCommands(ctx)
		if err != nil {
			log.Error("cannot load command queue", "error", err)
			continue
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return
			}
			c, wire, err := repo.ClaimCommand(ctx, id)
			if err != nil {
				log.Error("cannot claim command", "command_id", id, "error", err)
				continue
			}
			if wire == nil {
				continue
			}
			sendCtx, cancel := context.WithDeadline(ctx, c.DeadlineAt)
			attempted, sendErr := sender.SendCommand(sendCtx, c.AgentID, c.SessionID, wire)
			cancel()
			// Record delivery uncertainty even during shutdown. Never replay a claimed send.
			persist, stop := context.WithTimeout(context.Background(), 3*time.Second)
			err = repo.FinishCommandSend(persist, id, attempted, sendErr)
			stop()
			if err != nil {
				log.Error("cannot record command send outcome", "command_id", id, "error", err)
			}
		}
	}
}
