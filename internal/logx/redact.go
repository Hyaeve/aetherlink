package logx

import (
	"net/url"
	"regexp"
	"strings"
)

var queryCredential = regexp.MustCompile(`([?&])([^=\s?&]+)=([^&\s"'<>]*)`)
var headerCredential = regexp.MustCompile(`(?i)(\bToken\s*=\s*")[^"]*(")|(\bBearer\s+)[A-Za-z0-9._~+/\-=]+`)

func Redact(message string) string {
	message = queryCredential.ReplaceAllStringFunc(message, func(field string) string {
		parts := strings.SplitN(field[1:], "=", 2)
		key, err := url.QueryUnescape(parts[0])
		if err != nil {
			return field
		}
		switch strings.ToLower(key) {
		case "api_key", "apikey", "token", "access_token", "accesstoken", "x-emby-token", "aetherlink_ticket":
			return field[:1] + parts[0] + "=[REDACTED]"
		default:
			return field
		}
	})
	return headerCredential.ReplaceAllStringFunc(message, func(field string) string {
		if quote := strings.IndexByte(field, '"'); quote >= 0 {
			return field[:quote+1] + "[REDACTED]\""
		}
		return field[:strings.IndexAny(field, " \t")+1] + "[REDACTED]"
	})
}
