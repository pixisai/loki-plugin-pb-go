package managedplugin

import "github.com/rs/zerolog"

type Option func(*Client)

func WithLogger(logger zerolog.Logger) Option {
	return func(c *Client) {
		c.logger = logger
	}
}

func WithDirectory(directory string) Option {
	return func(c *Client) {
		c.directory = directory
	}
}

func WithNoSentry() Option {
	return func(c *Client) {
		c.noSentry = true
	}
}

func WithNoExec() Option {
	return func(c *Client) {
		c.noExec = true
	}
}

func WithOtelEndpoint(endpoint string) Option {
	return func(c *Client) {
		c.otelEndpoint = endpoint
	}
}

func WithOtelEndpointInsecure() Option {
	return func(c *Client) {
		c.otelEndpointInsecure = true
	}
}

func WithAuthToken(authToken string) Option {
	return func(c *Client) {
		c.authToken = authToken
	}
}

func WithTeamName(teamName string) Option {
	return func(c *Client) {
		c.teamName = teamName
	}
}
