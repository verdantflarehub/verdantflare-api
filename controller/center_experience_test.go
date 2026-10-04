package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCenterExperienceChatRejectsUnboundedOrInvalidInputBeforeRelay(t *testing.T) {
	for _, test := range []struct {
		name, organization, body string
		want                     int
	}{
		{name: "invalid organization", organization: "..", body: `{"modelId":"deepseek-flash","prompt":"hello"}`, want: http.StatusBadRequest},
		{name: "invalid json", organization: "org_test", body: `{`, want: http.StatusBadRequest},
		{name: "invalid model", organization: "org_test", body: `{"modelId":"https://upstream.test","prompt":"hello"}`, want: http.StatusBadRequest},
		{name: "empty prompt", organization: "org_test", body: `{"modelId":"deepseek-flash","prompt":"  "}`, want: http.StatusBadRequest},
		{name: "over character limit", organization: "org_test", body: `{"modelId":"deepseek-flash","prompt":"` + strings.Repeat("中", 2001) + `"}`, want: http.StatusBadRequest},
		{name: "over body limit", organization: "org_test", body: `{"modelId":"deepseek-flash","prompt":"` + strings.Repeat("a", 17000) + `"}`, want: http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(writer)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/internal/center/organizations/"+test.organization+"/experience/chat-completions", strings.NewReader(test.body))
			ctx.Params = gin.Params{{Key: "organizationID", Value: test.organization}}
			CenterExperienceChat(ctx)
			require.Equal(t, test.want, writer.Code)
		})
	}
}
