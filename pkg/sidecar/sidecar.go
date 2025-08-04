package sidecar

import (
	"context"
	"encoding/json"
	"github.com/aws/aws-sdk-go-v2/config"
	"go.uber.org/zap"
	"net/http"
	"time"
)

type CredentialResponse struct {
	AccessKeyID     string `json:"AccessKeyId"`
	Expiration      string
	SecretAccessKey string
	Token           string `json:"Token,omitempty"`
}

// New creates a handler that returns the current credentials obtained from the
// default credential chain, in a form suitable to act as the
// AWS_CONTAINER_CREDENTIALS_FULL_URI endpoint of another process.  The intended
// use case is when we want to run a pod that uses something like rclone
// or a similar tool that will be run many times in different processes, but
// does not have a native way to cache temporary credentials across calls.  We
// could configure AWS_WEB_IDENTITY_TOKEN_FILE etc. but this would mean that
// each rclone invocation would request a new set of temporary credentials from
// the STS endpoint.  Instead, we run this "sidecar" as a long-running process
// that retrieves new credentials only when the old ones expire, and configure
// the rclone container with
//
//	AWS_CONTAINER_CREDENTIALS_FULL_URI=http://localhost:8787/creds
//
// to cause it to fetch the (cached) credentials from us, exactly as it would
// when running in AWS Fargate.
func New(sugar *zap.SugaredLogger) http.Handler {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		sugar.Fatal(err)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		sugar.Info("Requested credentials")
		// cfg was created with LoadDefaultConfig, so we know it will be
		// wrapped in a CredentialsCache and it's fine to call Retrieve on
		// every request without making multiple calls to the real
		// credential provider
		creds, err := cfg.Credentials.Retrieve(request.Context())
		if err != nil {
			sugar.Warnw("Unable to retrieve credentials", "error", err)
			http.Error(w, err.Error(), 500)
			return
		}
		expiry := creds.Expires
		if !creds.CanExpire {
			// add a dummy expiry time if we have persistent rather than
			// temporary credentials
			expiry = time.Now().Add(time.Hour).UTC()
		}
		resp := CredentialResponse{
			AccessKeyID:     creds.AccessKeyID,
			Expiration:      expiry.Format(time.RFC3339),
			SecretAccessKey: creds.SecretAccessKey,
			Token:           creds.SessionToken,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(&resp); err != nil {
			http.Error(w, err.Error(), 500)
		}
	})
}
