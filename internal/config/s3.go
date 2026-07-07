package config

// S3 environment variable names. These are the single source of truth for both
// loading credentials (in a service or the sidecar) and forwarding them into
// the sidecar containers that actually run download/upload artifacts.
const (
	EnvS3Endpoint        = "S3_ENDPOINT"               // empty = AWS (region-derived host)
	EnvS3Region          = "S3_REGION"                 // SigV4 region; default us-east-1
	EnvS3AccessKeyID     = "S3_ACCESS_KEY_ID"          //
	EnvS3SecretAccessKey = "S3_SECRET_ACCESS_KEY"      // plain value (forwarded to sidecar)
	EnvS3SecretFile      = "S3_SECRET_ACCESS_KEY_FILE" // file path (preferred at rest)
	EnvS3ForcePathStyle  = "S3_FORCE_PATH_STYLE"       // "true" for MinIO/path-style
)

const defaultS3Region = "us-east-1"

// S3Credentials configures signing of s3:// download/upload artifacts. It is
// loaded per service (jobs vs deployments) from that service's environment and
// forwarded verbatim into the sidecar containers via ToEnv.
type S3Credentials struct {
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	ForcePathStyle  bool
}

// LoadS3Credentials reads S3 credentials from the environment. The secret is
// read from a mounted file when EnvS3SecretFile is set, else the plain env var.
func LoadS3Credentials() S3Credentials {
	secret := GetSecretFile(GetEnv(EnvS3SecretFile, ""))
	if secret == "" {
		secret = GetEnv(EnvS3SecretAccessKey, "")
	}
	return S3Credentials{
		Endpoint:        GetEnv(EnvS3Endpoint, ""),
		Region:          GetEnv(EnvS3Region, defaultS3Region),
		AccessKeyID:     GetEnv(EnvS3AccessKeyID, ""),
		SecretAccessKey: secret,
		ForcePathStyle:  GetEnv(EnvS3ForcePathStyle, "") == "true",
	}
}

// Enabled reports whether credentials are configured. When false, s3:// artifacts
// fail at apply time with a clear error.
func (c S3Credentials) Enabled() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// ToEnv returns the credentials as deterministic KEY=VALUE pairs for forwarding
// into a sidecar container. Empty when credentials are not configured, so the
// sidecar sees nothing unless the service is set up for S3.
func (c S3Credentials) ToEnv() [][2]string {
	if !c.Enabled() {
		return nil
	}
	env := [][2]string{
		{EnvS3AccessKeyID, c.AccessKeyID},
		{EnvS3SecretAccessKey, c.SecretAccessKey},
		{EnvS3Region, c.Region},
	}
	if c.Endpoint != "" {
		env = append(env, [2]string{EnvS3Endpoint, c.Endpoint})
	}
	if c.ForcePathStyle {
		env = append(env, [2]string{EnvS3ForcePathStyle, "true"})
	}
	return env
}
