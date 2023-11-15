package managedplugin

import "github.com/rs/zerolog"

func (c *Client) jsonToLog(l zerolog.Logger, msg map[string]any) {
	level := msg["level"]
	delete(msg, "level")
	switch level {
	case "trace":
		l.Trace().Fields(msg).Msg("")
	case "debug":
		l.Debug().Fields(msg).Msg("")
	case "info":
		l.Info().Fields(msg).Msg("")
	case "warn":
		l.Warn().Fields(msg).Msg("")
		c.metrics.incrementWarnings()
	case "error":
		l.Error().Fields(msg).Msg("")
		c.metrics.incrementErrors()
	default:
		l.Error().Fields(msg).Msg("unknown level")
	}
}
