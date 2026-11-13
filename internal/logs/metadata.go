package logs

import "nordik-drive-api/internal/activity"

func safeMetadata(metadata any) *string { return activity.SafeMetadata(metadata) }
