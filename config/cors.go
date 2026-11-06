package config

import (
	"os"
	"strings"
)

func AllowedOrigins() []string {
	if raw := strings.TrimSpace(os.Getenv("CORS_ORIGINS")); raw != "" {
		var origins []string
		for _, origin := range strings.Split(raw, ",") {
			if origin = strings.TrimSpace(origin); origin != "" {
				origins = append(origins, origin)
			}
		}
		return origins
	}
	return []string{"http://localhost:3000", "http://34.145.18.109/", "https://nordik-drive-react-724838782318.us-west1.run.app", "https://nordik-drive-react-prod-724838782318.us-west1.run.app"}
}
