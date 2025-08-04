package main

import (
	"flag"
	wh "github.com/GateNLP/minio-sts-k8s/pkg/webhook"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"os"
	"sigs.k8s.io/controller-runtime"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"time"
)

var (
	certDir         string
	scheme          = runtime.NewScheme()
	tokenMountPoint string
	tokenAudience   string
	tokenExpiration time.Duration
	roleArn         string
	region          string
	s3Endpoint      string
	stsEndpoint     string
	sidecarImage    string
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
}

func main() {
	crlog.SetLogger(zap.New())
	log := controllerruntime.Log.WithName("entrypoint")
	defaultExpiration := 15 * time.Minute
	if parsed, err := time.ParseDuration(os.Getenv("TOKEN_EXPIRATION")); err != nil {
		defaultExpiration = parsed
	}
	flag.StringVar(&certDir, "cert-dir", "/certs", "Directory containing TLS certificates for webhook (default /certs)")
	flag.StringVar(&tokenAudience, "audience", os.Getenv("TOKEN_AUDIENCE"), "Audience for the projected token")
	flag.StringVar(&tokenMountPoint, "token-mount-path", os.Getenv("TOKEN_MOUNT_PATH"), "Directory inside containers where \"token\" file should be projected")
	flag.StringVar(&roleArn, "role-arn", os.Getenv("AWS_ROLE_ARN"), "Role ARN to assume")
	flag.StringVar(&s3Endpoint, "s3-endpoint", os.Getenv("AWS_ENDPOINT_URL_S3"), "S3 endpoint to inject into pods")
	flag.StringVar(&stsEndpoint, "sts-endpoint", os.Getenv("AWS_ENDPOINT_URL_STS"), "STS endpoint to inject into pods")
	flag.StringVar(&region, "aws-region", os.Getenv("AWS_REGION"), "AWS region (default us-east-1)")
	flag.StringVar(&sidecarImage, "sidecar-image", os.Getenv("SIDECAR_IMAGE"), "Image reference for the injected sidecar container")
	flag.DurationVar(&tokenExpiration, "token-expiration", defaultExpiration, "Lifetime of service account token projected into pods, must be between 10m and 24h")

	flag.Parse()

	ctx := signals.SetupSignalHandler()

	handler := wh.NewMutator(scheme, tokenMountPoint, tokenAudience, int64(tokenExpiration.Seconds()), roleArn, stsEndpoint, s3Endpoint, region, sidecarImage)

	config := controllerruntime.GetConfigOrDie()
	mgr, err := controllerruntime.NewManager(config, controllerruntime.Options{
		Scheme:         scheme,
		LeaderElection: false,
		WebhookServer: webhook.NewServer(webhook.Options{
			CertDir: certDir,
		}),
		HealthProbeBindAddress: ":9440",
	})
	if err != nil {
		log.Error(err, "unable to set up controller manager")
		os.Exit(1)
	}
	mgr.GetWebhookServer().Register("/mutate-v1-pod", &webhook.Admission{Handler: handler})
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "unable to run manager")
	}
}
