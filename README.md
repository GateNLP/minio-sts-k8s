# Minio STS authentication using Kubernetes service accounts

A tool to simplify authentication to a Minio server using Kubernetes service account tokens.

## Background

[Minio](https://min.io) is an S3-compatible data store, which includes an implementation of the AWS STS `AssumeRoleWithWebIdentity` that allows clients to obtain and use temporary credentials using an OpenID Connect JWT token.  In recent versions of Kubernetes it is possible to generate ServiceAccount tokens that are valid JWTs, and the Kubernetes apiserver implements enough of the OIDC spec that Minio is able to verify the tokens and use them for authentication.  But doing so is tricky, with the need to inject the right volumes and environment variables into any pod that wants to use Minio, including the S3 and STS endpoints, the identity token, and a specific "role ARN" issued by Minio.

This tool provides a mutating webhook that allows all these values to be set in one central place (the webhook deployment); any pod that requires access to Minio can indicate this with a specific label and the webhook will inject the necessary settings dynamically.

## Prerequisites

If your Minio is deployed inside your cluster using the Minio Operator then you can use the [PolicyBinding mechanism](https://github.com/minio/operator/blob/master/docs/STS.md) with the Minio Operator STS endpoint to bind policies to service accounts.  This tool is intended for use when your Minio deployment is _outside_ your cluster and the PolicyBinding CRD is not an option.  In order for this to work a number of prerequisites must be met:

**1. Minio must be able to access your cluster's OIDC discovery document (and the JWKS key file that it references) without authentication**

The Kubernetes apiserver publishes an OIDC discovery document at `/.well-known/openid-configuration`, and the corresponding JWKS keys at `/openid/v1/jwks`, but by default these files are only accessible to authenticated users.  Minio must be able to access the discovery document and keys without authentication in order to validate cluster tokens.  There are two basic ways to achieve this:

1. Host a manually-edited copy of the discovery document and JWKS file at a different (public) https URL - this could even be in a Minio bucket configured for anonymous access
2. Configure the apiserver to permit unauthenticated access to the discovery document, and supply the cluster CA certificate to Minio so it will trust the apiserver certificate

For option 1, you will need to download the discovery document and JWKS file:

```shell
kubectl get --raw /.well-known/openid-configuration > discovery.json
kubectl get --raw /openid/v1/jwks > jwks.json
```

Edit the discovery document to set the `jwks_uri` to the location where you will publish the JWKS file, then place both files on a public https website that the Minio server is able to access.  Whenever your cluster rotates the token signing key you will need to fetch and publish the new version of the JWKS file accordingly.

For option 2, to permit unauthenticated access to these endpoints, you must bind the appropriate cluster role

```shell
kubectl create clusterrolebinding service-account-issuer-discovery-unauthenticated \
          --clusterrole=system:service-account-issuer-discovery \
          --group=system:unauthenticated
```

If your apiserver https endpoint uses a certificate that is not publicly trusted you will need to supply the cluster CA certificate to your minio server processes, e.g. by placing it in a file `/etc/minio/cacerts/cluster.crt` and passing the environment variable `SSL_CERT_DIR=/etc/minio/cacerts` to the `minio server` processes.

**2. Minio must be configured to trust the cluster as an IDP**

With an administrative Minio login, run:

```shell
mc idp openid add {alias} kubernetes \
    client_id=api://kubernetes \
    client_secret=dummy \
    config_url={discovery_url} \
    role_policy=kubernetes
```

where `{discovery_url}` is the URL of your discovery document - for "option 1" above this is wherever you hosted your modified file, for "option 2" it will be `{apiserver-base-url}/.well-known/openid-configuration`.  The value you choose for the `client_id` here will be specified as the "token audience" when setting up the webhook, and the `role_policy` is the name of the Minio _policy_ that will be applied to service account authentications.  This command will return a "role ARN" which you will need later.

All authentications using service account tokens will use the same policy document in Minio to determine their permissions, but you can control which statements within the policy apply to which service accounts by using conditions on the `jwt:sub`; for service account tokens this is always of the form `system:serviceaccount:{namespace}:{sa_name}`, so the following statement would apply for all serviceaccounts in the `ns1` namespace, and also all serviceaccounts in the `ns2` namespace whose name starts with `backup`:

```json
{
  "Effect": "Allow",
  "Action": "s3:*",
  "Resource": "arn:aws:s3:::backups",
  "Condition": {
    "StringLike": {
      "jwt:sub": [
        "system:serviceaccount:ns1:*",
        "system:serviceaccount:ns2:backup*"
      ]
    }
  }
}
```

## Authenticating with a serviceaccount token

Once Minio is set up to trust your cluster's tokens, actually using a token to authenticate to Minio using a standard AWS SDK (Java, Python boto3, Go, etc.) involves adding a number of things to your pods and containers:

- a `projected` volume with a `serviceAccountToken` whose `audience` matches the `client_id` you specified to Minio
- environment variables:
    - `AWS_WEB_IDENTITY_TOKEN_FILE` pointing to the mounted token
    - `AWS_ROLE_ARN` with the role ARN corresponding to the OIDC IDP in Minio
    - `AWS_ROLE_SESSION_NAME` - must be set, but any value will work
    - `AWS_ENDPOINT_URL_S3` and `AWS_ENDPOINT_URL_STS` for the endpoints of the S3 storage API and the Security Token Service that exchanges the JWT for temporary signing credentials - for Minio these must both be set to the root URL of your Minio deployment
    - `AWS_REGION` for the region configured for your Minio instance (default `us-east-1`)

Additionally, if your Minio S3 & STS endpoint does not use a publicly trusted certificate then you must supply the right CA certificate - exactly how this is achieved varies from SDK to SDK; for Go and Python you can use the environment variable `AWS_CA_BUNDLE`, for Java you must either programmatically override the `TrustManager` settings when creating your clients, or supply a custom keystore in place of the system-wide `cacerts` using the `javax.net.ssl.trustStore` system property.

## Admission webhook

This is a lot of boilerplate to enable token authentication, and if you have many deployments that require it then it is a lot of different places to keep in sync if anything changes.  To simplify things, this tool provides a _mutating admission webhook_ that looks for a specific label on pod specifications, and if found, adds the above projected volume and appropriate environment variables to the pod's containers.  Instead of all the above boilerplate, you simply add the label `sts.gate.ac.uk/use: "true"` to the pod template metadata, and your pod will be set up to be able to access Minio using any of the AWS SDKs.

> Note that the label goes on the _pod_; for a _deployment_ or _statefulset_ the label needs to be under `spec.template.metadata.labels`, not in the top level `metadata.labels`

Normally, if a pod is labelled `sts.gate.ac.uk/use: "true"` then _all_ containers and `initContainers` in the pod will have the token mounted at `/var/run/secrets/sts.gate.ac.uk/serviceaccount/token` and the above environment variables set.  Additional pod _annotations_ can be used to customise this behaviour:

- `sts.gate.ac.uk/only-containers` - a comma-separated list of _container_ names.  If set, _only_ the containers/initContainers with these names will have the token and environment variables injected.
- `sts.gate.ac.uk/except-containers` - a comma-separated list of container names.  This is the inverse of `only-containers` - if set, the token and environment will be injected into all containers & initContainers _except_ the ones with these names.

By default, each container will get the token and each process running in the container will request its own temporary credentials from STS.  If the pod has many containers that all need to access STS, or repeatedly invokes separate processes that each need to talk to Minio (e.g. a shell script making several calls to a tool such as `rclone`) then this can be very inefficient.  For cases like these you can specify the pod annotation `sts.gate.ac.uk/mode: "sidecar"`.  Instead of injecting the token into every container, this will run a separate _sidecar_ container that performs the actual token authentication on demand, and serves the temporary credentials via a local http server, and then configure the other containers to retrieve their credentials from this sidecar endpoint instead of directly from STS.  This way there is just one set of temporary credentials for the whole _pod_, rather than one per process.

By default, the sidecar listens on port 8787; if this clashes with a port in use by one of the pod's regular containers you can add the annotation `sts.gate.ac.uk/sidecar-port: "9876"` to specify an alternative port number for the sidecar.

> **Note**: if your Minio https certificate is not publicly trusted, then you are still responsible for adding the correct CA certificates to your own pods - as noted above there is no single standard approach for this that will work with all SDKs, so the webhook is not able to inject the CAs for you.

**Example**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: backups
  namespace: example
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: backups
  template:
    metadata:
      labels:
        app.kubernetes.io/name: backups
        # Inject the STS credentials
        sts.gate.ac.uk/use: "true"
    spec:
      containers:
        - name: run-backup
          image: example-image:latest
          command:
            - /backup-to-s3
          args:
            - --bucket
            - db-backups
```

If you were to create this Deployment, then inspect any of its pods, you would see the projected token volume and the additional environment variables in the pod spec.


## Installing the webhook

### (Step 0: prepare the sidecar)

The public sidecar image only includes the standard public CA certificates.  If your Minio certificate is not publicly trusted then you will need to build a custom image for your sidecar that includes the relevant configuration, e.g. using a Dockerfile like

```dockerfile
FROM ghcr.io/gatenlp/minio-sts-sidecar:latest
COPY minio-ca.crt /minio-certs/ca.crt
ENV AWS_CA_BUNDLE=/minio-certs/ca.crt
```

Build this image and push it to your own registry, then add the sidecar image location when configuring the webhook deployment.

### Step 1: create a namespace

The sample manifests assume the webhook is deployed into the `minio-sts` namespace:

```shell
kubectl create namespace minio-sts
```

### Step 2: generate a TLS certificate for the webhook

All admission webhooks called by the Kubernetes apiserver need to have a valid TLS server certificate, and the webhook registration must include the appropriate CA certificate for the apiserver to trust the connection.  The simplest way to handle this is using [cert-manager](https://cert-manager.io) (with an issuer such as `CA` or `vault` that is able to generate certificates for cluster-internal hostnames).

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: sts-webhook
  namespace: minio-sts
spec:
  secretName: sts-webhook-tls
  commonName: sts-webhook
  dnsNames:
    - sts-webhook
    - sts-webhook.minio-sts
    - sts-webhook.minio-sts.svc
  issuerRef:
    # Link your issuer here
```

If you do not have cert-manager then you will need to use some other mechanism to generate a certificate for the above dns names and store it in the `sts-webhook-tls` secret.

### Step 2: deploy the webhook

An example manifest for deploying the webhook is available at [deploy/sts-webhook.yaml](deploy/sts-webhook.yaml) - this will not deploy as-is, you **must** edit at least the audience, role ARN and endpoint URL settings before deploying.

Once the deployment is up and running, use [deploy/webhook-registration.yaml](deploy/webhook-registration.yaml) to register the hook with the Kubernetes apiserver.  The sample manifest assumes you are using cert-manager to provision the certificate, if not then you will need to edit the manifest to remove the `inject-ca-from` annotation and add the `caBundle` data by hand.
