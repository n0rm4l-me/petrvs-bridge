package transport

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func isAlreadyExists(err error) bool { return status.Code(err) == codes.AlreadyExists }
