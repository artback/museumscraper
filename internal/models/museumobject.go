package models

import (
	"github.com/minio/minio-go/v7/pkg/notification"
)

type MuseumObject struct {
	Data  *Museum
	Event notification.Event
}
