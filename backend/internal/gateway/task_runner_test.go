package gateway

import "testing"

func TestSanitizeTaskMessage(t *testing.T) {
	cases := []struct {
		name string
		task *TaskError
		want string
	}{
		{
			name: "invalid request keeps raw message",
			task: &TaskError{Type: "invalid_request", Message: "model does not support this size"},
			want: "model does not support this size",
		},
		{
			name: "rate limited is generic",
			task: &TaskError{Type: "rate_limited", Message: "The usage limit has been reached"},
			want: "too many requests, please retry later",
		},
		{
			name: "auth error is generic",
			task: &TaskError{Type: "auth_error", Message: "token invalid"},
			want: "account authentication failed, please contact the administrator",
		},
		{
			name: "upstream error is generic",
			task: &TaskError{Type: "upstream_error", Message: "server exploded"},
			want: "the request could not be completed right now, please retry later",
		},
		{
			name: "grpc desc is extracted",
			task: &TaskError{Type: "upstream_error", Message: "rpc error: code = Unknown desc = detailed reason"},
			want: "detailed reason",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeTaskMessage(tc.task); got != tc.want {
				t.Fatalf("sanitizeTaskMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}
