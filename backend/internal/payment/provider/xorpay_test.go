package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

func TestXorPayCreatePaymentPostsFaceToFaceAlipayOrder(t *testing.T) {
	var gotPath string
	var gotForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		gotForm = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"aoid":   "AO123",
			"info": map[string]any{
				"qr": "https://qr.alipay.com/test",
			},
			"expires_in": 7200,
		})
	}))
	defer server.Close()

	provider, err := NewXorPay("1", map[string]string{
		"aid":       "aid123",
		"appSecret": "secret",
		"apiBase":   server.URL,
		"notifyUrl": "https://example.com/api/v1/payment/webhook/xorpay",
	})
	if err != nil {
		t.Fatalf("NewXorPay: %v", err)
	}

	resp, err := provider.CreatePayment(context.Background(), payment.CreatePaymentRequest{
		OrderID:   "order-1",
		Amount:    "12.30",
		Subject:   "Sub2API 充值",
		NotifyURL: "https://notify.local/xorpay",
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}

	if gotPath != "/api/pay/aid123" {
		t.Fatalf("path = %q, want /api/pay/aid123", gotPath)
	}
	assertFormValue(t, gotForm, "name", "Sub2API 充值")
	assertFormValue(t, gotForm, "pay_type", "alipay")
	assertFormValue(t, gotForm, "price", "12.30")
	assertFormValue(t, gotForm, "order_id", "order-1")
	assertFormValue(t, gotForm, "notify_url", "https://notify.local/xorpay")
	wantSign := xorPayMD5("Sub2API 充值" + "alipay" + "12.30" + "order-1" + "https://notify.local/xorpay" + "secret")
	assertFormValue(t, gotForm, "sign", wantSign)

	if resp.TradeNo != "AO123" || resp.QRCode != "https://qr.alipay.com/test" || resp.PayURL != "https://qr.alipay.com/test" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestXorPayCreatePaymentAcceptsStringExpiresIn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     "ok",
			"aoid":       "AO123",
			"info":       "https://qr.alipay.com/test",
			"expires_in": "7200",
		})
	}))
	defer server.Close()

	provider, err := NewXorPay("1", map[string]string{
		"aid":       "aid123",
		"appSecret": "secret",
		"apiBase":   server.URL,
		"notifyUrl": "https://example.com/api/v1/payment/webhook/xorpay",
	})
	if err != nil {
		t.Fatalf("NewXorPay: %v", err)
	}

	resp, err := provider.CreatePayment(context.Background(), payment.CreatePaymentRequest{
		OrderID:   "order-1",
		Amount:    "12.30",
		Subject:   "Sub2API 充值",
		NotifyURL: "https://notify.local/xorpay",
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if resp.TradeNo != "AO123" || resp.QRCode != "https://qr.alipay.com/test" || resp.PayURL != "https://qr.alipay.com/test" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestXorPayVerifyNotificationUsesNotifySignature(t *testing.T) {
	provider, err := NewXorPay("1", map[string]string{
		"aid":       "aid123",
		"appSecret": "secret",
		"apiBase":   "https://xorpay.com",
		"notifyUrl": "https://example.com/api/v1/payment/webhook/xorpay",
	})
	if err != nil {
		t.Fatalf("NewXorPay: %v", err)
	}

	values := url.Values{}
	values.Set("aoid", "AO123")
	values.Set("order_id", "order-1")
	values.Set("pay_price", "12.30")
	values.Set("pay_time", "2026-05-18 20:00:00")
	values.Set("more", "user-9")
	values.Set("detail", `{"transaction_id":"tx123"}`)
	values.Set("sign", xorPayMD5("AO123"+"order-1"+"12.30"+"2026-05-18 20:00:00"+"secret"))

	notification, err := provider.VerifyNotification(context.Background(), values.Encode(), nil)
	if err != nil {
		t.Fatalf("VerifyNotification: %v", err)
	}
	if notification.TradeNo != "AO123" || notification.OrderID != "order-1" || notification.Amount != 12.30 || notification.Status != payment.ProviderStatusSuccess {
		t.Fatalf("unexpected notification: %+v", notification)
	}
	if notification.Metadata["more"] != "user-9" || notification.Metadata["detail"] != `{"transaction_id":"tx123"}` {
		t.Fatalf("unexpected metadata: %+v", notification.Metadata)
	}
}

func TestXorPayQueryOrderUsesOrderIDAndMapsStatus(t *testing.T) {
	var gotPath string
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    "success",
			"aoid":      "AO123",
			"pay_price": "12.30",
		})
	}))
	defer server.Close()

	provider, err := NewXorPay("1", map[string]string{
		"aid":       "aid123",
		"appSecret": "secret",
		"apiBase":   server.URL,
		"notifyUrl": "https://example.com/api/v1/payment/webhook/xorpay",
	})
	if err != nil {
		t.Fatalf("NewXorPay: %v", err)
	}

	resp, err := provider.QueryOrder(context.Background(), "order-1")
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if gotPath != "/api/query2/aid123" {
		t.Fatalf("path = %q, want /api/query2/aid123", gotPath)
	}
	if gotQuery.Get("order_id") != "order-1" {
		t.Fatalf("order_id query = %q", gotQuery.Get("order_id"))
	}
	if gotQuery.Get("sign") != xorPayMD5("order-1"+"secret") {
		t.Fatalf("unexpected sign query = %q", gotQuery.Get("sign"))
	}
	if resp.TradeNo != "AO123" || resp.Status != payment.ProviderStatusPaid || resp.Amount != 12.30 {
		t.Fatalf("unexpected query response: %+v", resp)
	}
}

func assertFormValue(t *testing.T, form url.Values, key, want string) {
	t.Helper()
	if got := form.Get(key); got != want {
		t.Fatalf("form[%s] = %q, want %q", key, got, want)
	}
}

// TestXorPayCreatePaymentOmitsReturnURLForUnsupportedPayTypes 验证：
// XorPay native / alipay 当面付 接口文档不支持 return_url 字段，
// 即使上层传入也不应该转发到上游（否则 XorPay 会返回 api_error）。
func TestXorPayCreatePaymentOmitsReturnURLForUnsupportedPayTypes(t *testing.T) {
	cases := []struct {
		name        string
		paymentType string
	}{
		{"wxpay-native", payment.TypeWxpay},
		{"wxpay-direct-native", payment.TypeWxpayDirect},
		{"alipay-face-to-face", payment.TypeAlipay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotForm url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Fatalf("ParseForm: %v", err)
				}
				gotForm = r.PostForm
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "ok",
					"aoid":   "AO123",
					"info":   map[string]any{"qr": "https://qr.example/test"},
				})
			}))
			defer server.Close()

			provider, err := NewXorPay("1", map[string]string{
				"aid":       "aid123",
				"appSecret": "secret",
				"apiBase":   server.URL,
				"notifyUrl": "https://example.com/api/v1/payment/webhook/xorpay",
				"returnUrl": "https://example.com/payment/result",
			})
			if err != nil {
				t.Fatalf("NewXorPay: %v", err)
			}

			_, err = provider.CreatePayment(context.Background(), payment.CreatePaymentRequest{
				OrderID:     "order-1",
				Amount:      "12.30",
				Subject:     "Sub2API 充值",
				NotifyURL:   "https://notify.local/xorpay",
				ReturnURL:   "https://app.example/payment/result?token=AAAA",
				PaymentType: tc.paymentType,
			})
			if err != nil {
				t.Fatalf("CreatePayment: %v", err)
			}

			if got := gotForm.Get("return_url"); got != "" {
				t.Fatalf("return_url should be omitted for pay_type %q, got %q", tc.paymentType, got)
			}
		})
	}
}

// TestXorPaySupportsReturnURL 校验 helper 行为，与 XorPay 文档保持一致。
func TestXorPaySupportsReturnURL(t *testing.T) {
	cases := map[string]bool{
		"native":         false,
		"alipay":         false,
		"jsapi":          true,
		"cashier":        true,
		"wechat_barcode": false,
		"alipay_barcode": false,
		"":               false,
	}
	for payType, want := range cases {
		if got := xorPaySupportsReturnURL(payType); got != want {
			t.Errorf("xorPaySupportsReturnURL(%q) = %v, want %v", payType, got, want)
		}
	}
}
