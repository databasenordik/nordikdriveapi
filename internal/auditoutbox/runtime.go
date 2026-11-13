package auditoutbox

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// RegisterOwner uses distinct method-specific identities for Audit reads/acks
// and Worker reconciliation. No browser token can call either operation.
func RegisterOwner(r *gin.Engine, db *gorm.DB, writer *Writer, audience string) error {
	if writer == nil {
		return nil
	}
	reader, err := GoogleAuthorize(audience, os.Getenv("AUDIT_READER_ACCOUNT"))
	if err != nil {
		return err
	}
	worker, err := GoogleAuthorize(audience, os.Getenv("AUDIT_WORKER_ACCOUNT"))
	if err != nil {
		return err
	}
	source := SourceHandler(db, reader)
	r.POST("/internal/v1/audit-outbox/read", gin.WrapH(source))
	r.POST("/internal/v1/audit-outbox/ack", gin.WrapH(source))
	r.POST("/internal/v1/audit-outbox/dispatch", gin.WrapH(DispatchHandler(&Dispatcher{DB: db, Publisher: writer.Publisher}, worker)))
	r.Use(func(c *gin.Context) {
		ctx, pending := Capture(c.Request.Context())
		c.Request = c.Request.WithContext(ctx)
		c.Next()
		publish, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if pending.Publish(publish, db, writer.Publisher) != nil {
			slog.WarnContext(publish, "audit_publication_incomplete")
		}
	})
	return nil
}
