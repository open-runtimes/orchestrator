package config

import (
	"log/slog"
	"regexp"
	"strings"
)

// EnvS3Profiles lists the named S3 profiles, comma-separated. Each profile
// reads the same variables as the default one, prefixed with its upper-cased
// name: profile "archive" reads S3_ARCHIVE_ACCESS_KEY_ID and so on.
const EnvS3Profiles = "S3_PROFILES"

// S3 variable suffixes. The default profile reads them as S3_<KEY>, a named
// profile as S3_<NAME>_<KEY>. Only the services read them: pods get grants
// (artifact.Grants), never these keys.
const (
	s3Endpoint        = "ENDPOINT"               // empty = AWS (region-derived host)
	s3Region          = "REGION"                 // SigV4 region; default us-east-1
	s3AccessKeyID     = "ACCESS_KEY_ID"          //
	s3SecretAccessKey = "SECRET_ACCESS_KEY"      // plain value
	s3SecretFile      = "SECRET_ACCESS_KEY_FILE" // file path (preferred at rest)
	s3SessionToken    = "SESSION_TOKEN"          // STS/temporary-credential session token
	s3SessionFile     = "SESSION_TOKEN_FILE"     // file path (for rotated STS creds)
	s3ForcePathStyle  = "FORCE_PATH_STYLE"       // "true" for MinIO/path-style
)

const defaultS3Region = "us-east-1"

// s3ProfileName is what a profile may be called: lower case, so the URL, the
// env var prefix and the chart all name it the same way.
var s3ProfileName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ValidS3ProfileName reports whether name can name an S3 profile.
func ValidS3ProfileName(name string) bool { return s3ProfileName.MatchString(name) }

// s3Env returns the environment variable holding key for a profile; "" is the
// default profile.
func s3Env(profile, key string) string {
	if profile == "" {
		return "S3_" + key
	}
	return "S3_" + strings.ToUpper(profile) + "_" + key
}

// S3Credentials configures signing of s3:// download/upload artifacts.
type S3Credentials struct {
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // set for STS/temporary credentials; signed as X-Amz-Security-Token
	ForcePathStyle  bool
}

// S3Profiles maps a profile name to its credentials. The default profile is
// "", used by s3://bucket/key; s3://name@bucket/key selects a named one. It is
// loaded per service (jobs vs deployments) from that service's environment and
// forwarded into the sidecar containers via ToEnv.
type S3Profiles map[string]S3Credentials

// LoadS3Profiles reads the default profile and every profile named in
// EnvS3Profiles from the environment. A name that is not a valid profile name
// is skipped, and a listed profile without keys is kept disabled; both log a
// warning, since an artifact naming either fails at apply time.
func LoadS3Profiles() S3Profiles {
	profiles := S3Profiles{"": loadS3Credentials("")}
	for name := range strings.SplitSeq(GetEnv(EnvS3Profiles, ""), ",") {
		name = strings.TrimSpace(name)
		switch {
		case name == "":
			continue
		case !ValidS3ProfileName(name):
			slog.Warn("Ignoring invalid S3 profile name", "profile", name, "want", s3ProfileName.String())
			continue
		}
		profiles[name] = loadS3Credentials(name)
		if !profiles[name].Enabled() {
			slog.Warn("S3 profile has no access key or secret", "profile", name)
		}
	}
	return profiles
}

// loadS3Credentials reads one profile. The secret is read from a mounted file
// when its _FILE variable is set, else the plain env var.
func loadS3Credentials(profile string) S3Credentials {
	env := func(key string) string { return GetEnv(s3Env(profile, key), "") }
	secret := GetSecretFile(env(s3SecretFile))
	if secret == "" {
		secret = env(s3SecretAccessKey)
	}
	session := GetSecretFile(env(s3SessionFile))
	if session == "" {
		session = env(s3SessionToken)
	}
	return S3Credentials{
		Endpoint:        env(s3Endpoint),
		Region:          GetEnv(s3Env(profile, s3Region), defaultS3Region),
		AccessKeyID:     env(s3AccessKeyID),
		SecretAccessKey: secret,
		SessionToken:    session,
		ForcePathStyle:  GetBoolEnv(s3Env(profile, s3ForcePathStyle), false),
	}
}

// Enabled reports whether credentials are configured. When false, s3:// artifacts
// fail at apply time with a clear error.
func (c S3Credentials) Enabled() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != ""
}
