package awsbedrock

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/personal/broxy/internal/config"
)

func (c *Client) logAWSAuth(ctx context.Context, cfg aws.Config) {
	if c.logger == nil {
		return
	}
	sources := credentialProviderSources(cfg.Credentials)
	fields := []any{
		"mode", config.UpstreamAuthAWS,
		"region", c.upstream.Region,
	}
	if c.upstream.Profile != "" {
		fields = append(fields, "profile", c.upstream.Profile)
	}
	if len(sources) > 0 {
		fields = append(fields, "credential_sources", strings.Join(credentialSourceNames(sources), ", "))
	}

	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	creds, err := cfg.Credentials.Retrieve(checkCtx)
	if err != nil {
		c.logger.Warn("bedrock credential check failed", append(fields,
			"auth_method", awsAuthMethod(sources, ""),
			"error", err,
		)...)
		return
	}

	fields = append(fields,
		"auth_method", awsAuthMethod(sources, creds.Source),
		"sdk_source", sanitizeCredentialSource(creds.Source),
		"temporary", creds.CanExpire,
		"session_token", creds.SessionToken != "",
	)
	if creds.CanExpire {
		fields = append(fields, "expires_at", creds.Expires.Format(time.RFC3339))
	}
	if creds.AccountID != "" {
		fields = append(fields, "account_id", creds.AccountID)
	}
	c.logger.Info("bedrock auth configured", fields...)
}

func (c *Client) logBearerAuth() {
	if c.logger == nil {
		return
	}
	fields := []any{
		"mode", config.UpstreamAuthBearer,
		"region", c.upstream.Region,
		"auth_method", "Bedrock API key",
		"token_configured", strings.TrimSpace(c.upstream.BearerToken) != "",
		"endpoint_override", c.upstream.EndpointOverride != "",
	}
	if strings.TrimSpace(c.upstream.BearerToken) == "" {
		c.logger.Warn("bedrock bearer auth selected without token", fields...)
		return
	}
	c.logger.Info("bedrock auth configured", fields...)
}

func credentialProviderSources(provider aws.CredentialsProvider) []aws.CredentialSource {
	sourceProvider, ok := provider.(aws.CredentialProviderSource)
	if !ok {
		return nil
	}
	return sourceProvider.ProviderSources()
}

func credentialSourceNames(sources []aws.CredentialSource) []string {
	names := make([]string, 0, len(sources))
	for _, source := range sources {
		names = append(names, credentialSourceName(source))
	}
	return names
}

func credentialSourceName(source aws.CredentialSource) string {
	switch source {
	case aws.CredentialSourceCode:
		return "code"
	case aws.CredentialSourceEnvVars:
		return "environment variables"
	case aws.CredentialSourceEnvVarsSTSWebIDToken:
		return "environment web identity token"
	case aws.CredentialSourceSTSAssumeRole:
		return "STS assume role"
	case aws.CredentialSourceSTSAssumeRoleSaml:
		return "STS assume role with SAML"
	case aws.CredentialSourceSTSAssumeRoleWebID:
		return "STS assume role with web identity"
	case aws.CredentialSourceSTSFederationToken:
		return "STS federation token"
	case aws.CredentialSourceSTSSessionToken:
		return "STS session token"
	case aws.CredentialSourceProfile:
		return "shared profile credentials"
	case aws.CredentialSourceProfileSourceProfile:
		return "source profile"
	case aws.CredentialSourceProfileNamedProvider:
		return "profile credential_source"
	case aws.CredentialSourceProfileSTSWebIDToken:
		return "profile web identity token"
	case aws.CredentialSourceProfileSSO:
		return "profile SSO session"
	case aws.CredentialSourceSSO:
		return "AWS SSO"
	case aws.CredentialSourceProfileSSOLegacy:
		return "legacy profile SSO"
	case aws.CredentialSourceSSOLegacy:
		return "legacy AWS SSO"
	case aws.CredentialSourceProfileProcess:
		return "profile credential_process"
	case aws.CredentialSourceProcess:
		return "credential_process"
	case aws.CredentialSourceHTTP:
		return "container credentials endpoint"
	case aws.CredentialSourceIMDS:
		return "instance metadata service"
	case aws.CredentialSourceProfileLogin:
		return "profile login session"
	case aws.CredentialSourceLogin:
		return "AWS login"
	default:
		return fmt.Sprintf("credential source %d", source)
	}
}

func awsAuthMethod(sources []aws.CredentialSource, sdkSource string) string {
	switch {
	case hasCredentialSource(sources, aws.CredentialSourceSTSAssumeRole):
		return "AWS STS assume role"
	case hasCredentialSource(sources, aws.CredentialSourceProfileSSO),
		hasCredentialSource(sources, aws.CredentialSourceSSO),
		hasCredentialSource(sources, aws.CredentialSourceProfileSSOLegacy),
		hasCredentialSource(sources, aws.CredentialSourceSSOLegacy):
		return "AWS SSO"
	case hasCredentialSource(sources, aws.CredentialSourceEnvVars):
		return "AWS access keys from environment"
	case hasCredentialSource(sources, aws.CredentialSourceProfile):
		return "AWS shared profile credentials"
	case hasCredentialSource(sources, aws.CredentialSourceProfileProcess),
		hasCredentialSource(sources, aws.CredentialSourceProcess):
		return "AWS credential_process"
	case hasCredentialSource(sources, aws.CredentialSourceEnvVarsSTSWebIDToken),
		hasCredentialSource(sources, aws.CredentialSourceProfileSTSWebIDToken),
		hasCredentialSource(sources, aws.CredentialSourceSTSAssumeRoleWebID):
		return "AWS web identity"
	case hasCredentialSource(sources, aws.CredentialSourceHTTP):
		return "AWS container credentials"
	case hasCredentialSource(sources, aws.CredentialSourceIMDS):
		return "AWS instance metadata credentials"
	}

	switch {
	case strings.Contains(sdkSource, "AssumeRoleProvider"):
		return "AWS STS assume role"
	case strings.Contains(sdkSource, "SSOProvider"):
		return "AWS SSO"
	case strings.Contains(sdkSource, "EnvConfigCredentials"):
		return "AWS access keys from environment"
	case strings.Contains(sdkSource, "SharedConfigCredentials"):
		return "AWS shared profile credentials"
	case strings.Contains(sdkSource, "ProcessProvider"):
		return "AWS credential_process"
	case strings.Contains(sdkSource, "WebIdentity"):
		return "AWS web identity"
	case strings.Contains(sdkSource, "EndpointCredentials"):
		return "AWS container credentials"
	case strings.Contains(sdkSource, "EC2RoleProvider"):
		return "AWS instance metadata credentials"
	default:
		return "AWS default credential chain"
	}
}

func hasCredentialSource(sources []aws.CredentialSource, needle aws.CredentialSource) bool {
	for _, source := range sources {
		if source == needle {
			return true
		}
	}
	return false
}

func sanitizeCredentialSource(source string) string {
	if strings.HasPrefix(source, "SharedConfigCredentials:") {
		return "SharedConfigCredentials"
	}
	if strings.TrimSpace(source) == "" {
		return "unknown"
	}
	return source
}
