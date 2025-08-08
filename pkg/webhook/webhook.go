package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	// UseLabel is the label we look for to know whether a pod needs STS credentials at all
	UseLabel = "sts.gate.ac.uk/use"

	// AnnMode is the annotation specifying whether to inject config for containers to
	// do their own AssumeRoleWithWebIdentity, or to run a sidecar that talks to STS and
	// makes the temporary credentials available via AWS_CONTAINER_CREDENTIALS_FULL_URI
	AnnMode     = "sts.gate.ac.uk/mode"
	ModeSidecar = "sidecar"
	ModeDirect  = "direct"

	SidecarContainerName = "gate-sts-sidecar"
	DefaultSidecarImage  = "ghcr.io/gatenlp/minio-sts-sidecar:latest"
	AnnSidecarPort       = "sts.gate.ac.uk/sidecar-port"
	DefaultSidecarPort   = "8787"
	EnvSidecarPort       = "SIDECAR_PORT"

	AnnOnlyContainers   = "sts.gate.ac.uk/only-containers"
	AnnExceptContainers = "sts.gate.ac.uk/except-containers"

	TokenVolumeName        = "gate-sts-token"
	TokenFileName          = "token"
	DefaultTokenMountPoint = "/var/run/secrets/sts.gate.ac.uk/serviceaccount"

	AnnTokenLifetime = "sts.gate.ac.uk/token-lifetime"

	EnvWebIdentityTokenFile = "AWS_WEB_IDENTITY_TOKEN_FILE"
	EnvRoleArn              = "AWS_ROLE_ARN"
	EnvRoleSession          = "AWS_ROLE_SESSION_NAME"
	EnvS3Endpoint           = "AWS_ENDPOINT_URL_S3"
	EnvSTSEndpoint          = "AWS_ENDPOINT_URL_STS"
	EnvAwsRegion            = "AWS_REGION"
	DefaultRegion           = "us-east-1"

	EnvContainerCredentialsUri = "AWS_CONTAINER_CREDENTIALS_FULL_URI"
)

type podMutator struct {
	decoder         admission.Decoder
	mountPoint      string
	tokenAudience   string
	tokenExpiration int64
	roleArn         string
	s3Endpoint      string
	stsEndpoint     string
	region          string
	sidecarImage    string
}

func NewMutator(scheme *runtime.Scheme, mountPoint string, audience string, tokenExpiration int64, roleArn string, stsEndpoint, s3Endpoint, region string, sidecarImage string) admission.Handler {
	if mountPoint == "" {
		mountPoint = DefaultTokenMountPoint
	}
	if tokenExpiration < 600 {
		tokenExpiration = 600
	}
	if tokenExpiration > 86400 {
		tokenExpiration = 86400
	}
	if sidecarImage == "" {
		sidecarImage = DefaultSidecarImage
	}
	if region == "" {
		region = DefaultRegion
	}
	log := crlog.Log.WithName("sts-webhook")
	log.Info("creating webhook handler", "mountPoint", mountPoint, "audience", audience, "roleArn", roleArn, "expiration", tokenExpiration, "stsEndpoint", stsEndpoint, "s3Endpoint", s3Endpoint, "region", region, "sidecarImage", sidecarImage)
	return &podMutator{
		decoder:         admission.NewDecoder(scheme),
		mountPoint:      mountPoint,
		tokenAudience:   audience,
		tokenExpiration: tokenExpiration,
		roleArn:         roleArn,
		stsEndpoint:     stsEndpoint,
		s3Endpoint:      s3Endpoint,
		region:          region,
		sidecarImage:    sidecarImage,
	}
}

func (m *podMutator) Handle(ctx context.Context, req admission.Request) (response admission.Response) {
	log := crlog.FromContext(ctx)
	pod := &corev1.Pod{}
	err := m.decoder.Decode(req, pod)
	if err != nil {
		log.Error(err, "unable to decode pod for mutation")
		return admission.Errored(http.StatusBadRequest, err)
	}
	log = log.WithValues("pod", pod.Name, "namespace", req.Namespace)

	// short circuit if this pod does not require our additions
	if pod.Labels == nil || pod.Labels[UseLabel] != "true" {
		return admission.Allowed("Pod has not opted in to STS credentials")
	}

	var containers []*corev1.Container = nil
	if pod.Annotations != nil {
		if only := pod.Annotations[AnnOnlyContainers]; only != "" {
			onlyContainers := make(map[string]bool)
			for _, c := range strings.Split(only, ",") {
				onlyContainers[strings.TrimSpace(c)] = true
			}

			containers = make([]*corev1.Container, 0, len(onlyContainers))
			for i := range pod.Spec.Containers {
				if onlyContainers[pod.Spec.Containers[i].Name] {
					containers = append(containers, &pod.Spec.Containers[i])
				}
			}
			for i := range pod.Spec.InitContainers {
				if onlyContainers[pod.Spec.InitContainers[i].Name] {
					containers = append(containers, &pod.Spec.InitContainers[i])
				}
			}
			if len(containers) == 0 {
				return admission.Allowed(fmt.Sprintf("%s specified, but no containers match - nothing to do", AnnOnlyContainers))
			}
		} else if except := pod.Annotations[AnnExceptContainers]; except != "" {
			exceptContainers := make(map[string]bool)
			for _, c := range strings.Split(except, ",") {
				exceptContainers[strings.TrimSpace(c)] = true
			}

			containers = make([]*corev1.Container, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
			for i := range pod.Spec.Containers {
				if !exceptContainers[pod.Spec.Containers[i].Name] {
					containers = append(containers, &pod.Spec.Containers[i])
				}
			}
			for i := range pod.Spec.InitContainers {
				if !exceptContainers[pod.Spec.InitContainers[i].Name] {
					containers = append(containers, &pod.Spec.InitContainers[i])
				}
			}
			if len(containers) == 0 {
				return admission.Allowed(fmt.Sprintf("%s specified, but all containers were excluded - nothing to do", AnnExceptContainers))
			}
		}
	}
	if containers == nil {
		// no only/except restrictions, so use all containers
		containers = make([]*corev1.Container, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
		for i := range pod.Spec.Containers {
			containers = append(containers, &pod.Spec.Containers[i])
		}
		for i := range pod.Spec.InitContainers {
			containers = append(containers, &pod.Spec.InitContainers[i])
		}
	}
	if len(containers) == 0 {
		return admission.Allowed("No containers in pod - nothing to do")
	}

	log.Info("Pod requires mutation", "containers", len(containers))

	if pod.Annotations != nil && pod.Annotations[AnnMode] == ModeSidecar {
		err = m.injectAsSidecar(pod, containers)
	} else {
		err = m.injectDirect(pod, containers)
	}

	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		log.Error(err, "unable to marshal mutated pod")
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// injectAsSidecar adds the sidecar container to the pod, then adds environment
// variables to each container in `containers` that will cause an AWS SDK in
// that container to request credentials from the sidecar.
func (m *podMutator) injectAsSidecar(pod *corev1.Pod, containers []*corev1.Container) error {
	hasSidecar := false
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == SidecarContainerName {
			// sidecar container has already been added
			hasSidecar = true
			break
		}
	}
	m.addTokenVolume(pod)

	sidecarPort := DefaultSidecarPort
	if pod.Annotations != nil && pod.Annotations[AnnSidecarPort] != "" {
		sidecarPort = pod.Annotations[AnnSidecarPort]
	}

	var sidecar *corev1.Container
	if !hasSidecar {
		restartPolicy := corev1.ContainerRestartPolicy("Always")
		varFalse := false
		varTrue := true
		var uid int64 = 65532
		var gid int64 = 65532
		sidecar = &corev1.Container{
			Name:            SidecarContainerName,
			Image:           m.sidecarImage,
			ImagePullPolicy: "IfNotPresent",
			RestartPolicy:   &restartPolicy,
			// we need a startup probe so that the other containers that might
			// call the credentials endpoint do not start up until the endpoint
			// is listening.  The obvious way to do this would be an httpGet
			// probe, but we can't use one of those because we only want the
			// sidecar to listen on the pod-internal 127.0.0.1 address (otherwise
			// anything in the cluster would be able to access the storage server
			// with the rights of this pod's service account by calling the
			// sidecar endpoint).  So the sidecar binary implements its own
			// probe mode that calls the probe endpoint on 127.0.0.1 and exits
			// 0 only when the endpoint is up, and we use this via an exec probe.
			StartupProbe: &corev1.Probe{
				SuccessThreshold: 1,
				FailureThreshold: 30,
				PeriodSeconds:    1,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{"/sts-sidecar", "probe"},
					},
				},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &varFalse,
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{"ALL"},
				},
				ReadOnlyRootFilesystem: &varTrue,
				RunAsNonRoot:           &varTrue,
				RunAsUser:              &uid,
				RunAsGroup:             &gid,
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
		}

		if sidecarPort != DefaultSidecarPort {
			sidecar.Env = []corev1.EnvVar{
				{Name: EnvSidecarPort, Value: sidecarPort},
			}
		}
		// inject into the sidecar the token and env vars needed to authenticate direct
		if err := m.injectDirect(pod, []*corev1.Container{sidecar}); err != nil {
			return err
		}
	}

	// inject into the other containers the env vars needed to authenticate using the sidecar
	for _, c := range containers {
		if c.Name != SidecarContainerName {
			m.injectEnvironmentSidecar(c, sidecarPort)
		}
	}

	if sidecar != nil {
		// prepend the sidecar to the pod, so it starts before any other init containers
		// (which may themselves need to access the STS credentials)
		newContainers := make([]corev1.Container, 0, len(pod.Spec.InitContainers)+1)
		newContainers = append(newContainers, *sidecar)
		newContainers = append(newContainers, pod.Spec.InitContainers...)
		pod.Spec.InitContainers = newContainers
	}
	return nil
}

// injectDirect adds the service account token projected volume to the pod, and
// adds environment variables to each container in `containers` that will cause
// an AWS SDK to use that token to request temporary credentials from STS.
func (m *podMutator) injectDirect(pod *corev1.Pod, containers []*corev1.Container) error {
	m.addTokenVolume(pod)
	for _, c := range containers {
		m.injectToContainer(c, pod.Name)
	}

	return nil
}

func (m *podMutator) addTokenVolume(pod *corev1.Pod) {
	for _, vol := range pod.Spec.Volumes {
		if vol.Name == TokenVolumeName {
			// there is already a volume of the desired name
			return
		}
	}

	expirationSeconds := m.tokenExpiration
	if pod.Annotations != nil {
		expiryFromAnn := pod.Annotations[AnnTokenLifetime]
		val, err := time.ParseDuration(expiryFromAnn)
		if err == nil {
			secondsFromAnn := int64(val.Seconds())
			if secondsFromAnn >= 600 && secondsFromAnn <= 86400 {
				expirationSeconds = secondsFromAnn
			}
		}
	}

	vol := corev1.Volume{
		Name: TokenVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Audience:          m.tokenAudience,
							ExpirationSeconds: &expirationSeconds,
							Path:              TokenFileName,
						},
					},
				},
			},
		},
	}

	pod.Spec.Volumes = append(pod.Spec.Volumes, vol)
}

func (m *podMutator) injectToContainer(c *corev1.Container, podName string) {
	m.ensureVolumeMount(c)
	m.injectEnvironmentDirect(c, podName)
}

func (m *podMutator) ensureVolumeMount(c *corev1.Container) {
	existingMount := false
	for _, mount := range c.VolumeMounts {
		if mount.Name == TokenVolumeName && mount.MountPath == m.mountPoint {
			existingMount = true
			break
		}
	}
	if !existingMount {
		// add mount
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      TokenVolumeName,
			MountPath: m.mountPoint,
			ReadOnly:  true,
		})
	}
}

func (m *podMutator) injectEnvironmentDirect(c *corev1.Container, podName string) {
	desiredEnvs := []corev1.EnvVar{
		{Name: EnvWebIdentityTokenFile, Value: fmt.Sprintf("%s/%s", m.mountPoint, TokenFileName)},
		{Name: EnvRoleArn, Value: m.roleArn},
		{Name: EnvRoleSession, Value: fmt.Sprintf("%s-%s", podName, c.Name)},
		{Name: EnvS3Endpoint, Value: m.s3Endpoint},
		{Name: EnvSTSEndpoint, Value: m.stsEndpoint},
		{Name: EnvAwsRegion, Value: m.region},
	}
	injectEnvironment(c, desiredEnvs)
}

func (m *podMutator) injectEnvironmentSidecar(c *corev1.Container, port string) {
	desiredEnvs := []corev1.EnvVar{
		{Name: EnvContainerCredentialsUri, Value: fmt.Sprintf("http://127.0.0.1:%s/creds", port)},
		{Name: EnvS3Endpoint, Value: m.s3Endpoint},
		{Name: EnvAwsRegion, Value: m.region},
	}
	injectEnvironment(c, desiredEnvs)
}

func injectEnvironment(c *corev1.Container, desiredEnvs []corev1.EnvVar) {
	currentEnv := make(map[string]string)
	for _, e := range c.Env {
		currentEnv[e.Name] = e.Value
	}

	for _, env := range desiredEnvs {
		if _, ok := currentEnv[env.Name]; !ok && env.Value != "" {
			c.Env = append(c.Env, env)
		}
	}

}
