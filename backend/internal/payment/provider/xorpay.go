package provider

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

const (
	xorPayDefaultAPIBase    = "https://xorpay.com"
	xorPayHTTPTimeout       = 10 * time.Second
	maxXorPayResponseSize   = 1 << 20
	maxXorPayErrorSummary   = 512
	xorPayStatusOK          = "ok"
	xorPayStatusNew         = "new"
	xorPayStatusPayed       = "payed"
	xorPayStatusSuccess     = "success"
	xorPayStatusExpire      = "expire"
	xorPayStatusNotExist    = "not_exist"
	xorPayPayTypeAlipay     = "alipay"
	xorPayPayTypeNative     = "native"
	xorPayDefaultExpireSecs = "7200"
)

// XorPay implements payment.Provider for XorPay.
type XorPay struct {
	instanceID string
	config     map[string]string
	httpClient *http.Client
}

// NewXorPay creates a new XorPay provider.
// config keys: aid, appSecret, notifyUrl, apiBase(optional), expire(optional)
func NewXorPay(instanceID string, config map[string]string) (*XorPay, error) {
	for _, k := range []string{"aid", "appSecret", "notifyUrl"} {
		if strings.TrimSpace(config[k]) == "" {
			return nil, fmt.Errorf("xorpay config missing required key: %s", k)
		}
	}
	cfg := make(map[string]string, len(config)+1)
	for k, v := range config {
		cfg[k] = strings.TrimSpace(v)
	}
	cfg["apiBase"] = normalizeXorPayAPIBase(cfg["apiBase"])
	if cfg["apiBase"] == "" {
		cfg["apiBase"] = xorPayDefaultAPIBase
	}
	return &XorPay{
		instanceID: instanceID,
		config:     cfg,
		httpClient: &http.Client{Timeout: xorPayHTTPTimeout},
	}, nil
}

func normalizeXorPayAPIBase(apiBase string) string {
	base := strings.TrimSpace(apiBase)
	if base == "" {
		return ""
	}
	if parsed, err := url.Parse(base); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		parsed.RawPath = ""
		parsed.Path = trimXorPayEndpointPath(parsed.Path)
		return strings.TrimRight(parsed.String(), "/")
	}
	return strings.TrimRight(trimXorPayEndpointPath(base), "/")
}

func trimXorPayEndpointPath(path string) string {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	lower := strings.ToLower(path)
	for _, marker := range []string{"/api/pay/", "/api/query/", "/api/query2/", "/api/refund/"} {
		if idx := strings.Index(lower, marker); idx >= 0 {
			return strings.TrimRight(path[:idx], "/")
		}
	}
	return path
}

func (x *XorPay) apiBase() string {
	if x == nil {
		return ""
	}
	return normalizeXorPayAPIBase(x.config["apiBase"])
}

func (x *XorPay) Name() string        { return "XorPay" }
func (x *XorPay) ProviderKey() string { return payment.TypeXorPay }
func (x *XorPay) SupportedTypes() []payment.PaymentType {
	return []payment.PaymentType{payment.TypeAlipay, payment.TypeWxpay}
}

func (x *XorPay) MerchantIdentityMetadata() map[string]string {
	if x == nil {
		return nil
	}
	aid := strings.TrimSpace(x.config["aid"])
	if aid == "" {
		return nil
	}
	return map[string]string{"aid": aid}
}

func (x *XorPay) CreatePayment(ctx context.Context, req payment.CreatePaymentRequest) (*payment.CreatePaymentResponse, error) {
	notifyURL := strings.TrimSpace(req.NotifyURL)
	if notifyURL == "" {
		notifyURL = x.config["notifyUrl"]
	}
	payType := xorPayResolvePayType(req.PaymentType)
	params := map[string]string{
		"name":       req.Subject,
		"pay_type":   payType,
		"price":      req.Amount,
		"order_id":   req.OrderID,
		"notify_url": notifyURL,
	}
	if req.ReturnURL != "" {
		params["return_url"] = req.ReturnURL
	} else if x.config["returnUrl"] != "" {
		params["return_url"] = x.config["returnUrl"]
	}
	if req.OpenID != "" {
		params["openid"] = req.OpenID
	}
	if expire := strings.TrimSpace(x.config["expire"]); expire != "" {
		params["expire"] = expire
	}
	params["sign"] = xorPayCreateSign(req.Subject, payType, req.Amount, req.OrderID, notifyURL, x.config["appSecret"])

	endpoint := fmt.Sprintf("%s/api/pay/%s", x.apiBase(), url.PathEscape(x.config["aid"]))
	body, status, err := x.postForm(ctx, endpoint, params)
	if err != nil {
		return nil, fmt.Errorf("xorpay create: %w", err)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("xorpay create HTTP %d: %s", status, summarizeXorPayResponse(body))
	}
	var resp struct {
		Status    string          `json:"status"`
		AOID      string          `json:"aoid"`
		Info      any             `json:"info"`
		ExpiresIn json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("xorpay parse: %w", err)
	}
	if resp.Status != xorPayStatusOK {
		return nil, fmt.Errorf("xorpay error: %s%s", resp.Status, xorPayInfoMessage(resp.Info))
	}
	qr := xorPayExtractQRCode(resp.Info)
	return &payment.CreatePaymentResponse{TradeNo: resp.AOID, PayURL: qr, QRCode: qr}, nil
}

func xorPayResolvePayType(paymentType string) string {
	switch strings.TrimSpace(paymentType) {
	case payment.TypeWxpay, payment.TypeWxpayDirect:
		return xorPayPayTypeNative
	default:
		return xorPayPayTypeAlipay
	}
}

func xorPayExtractQRCode(info any) string {
	switch v := info.(type) {
	case map[string]any:
		if qr, ok := v["qr"].(string); ok {
			return strings.TrimSpace(qr)
		}
	case string:
		return strings.TrimSpace(v)
	}
	return ""
}

func xorPayInfoMessage(info any) string {
	if info == nil {
		return ""
	}
	b, err := json.Marshal(info)
	if err != nil {
		return fmt.Sprintf(": %v", info)
	}
	return ": " + string(b)
}

func (x *XorPay) QueryOrder(ctx context.Context, tradeNo string) (*payment.QueryOrderResponse, error) {
	orderID := strings.TrimSpace(tradeNo)
	q := url.Values{}
	q.Set("order_id", orderID)
	q.Set("sign", xorPayMD5(orderID+x.config["appSecret"]))
	endpoint := fmt.Sprintf("%s/api/query2/%s?%s", x.apiBase(), url.PathEscape(x.config["aid"]), q.Encode())
	body, status, err := x.get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("xorpay query: %w", err)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("xorpay query HTTP %d: %s", status, summarizeXorPayResponse(body))
	}
	var resp struct {
		Status   string      `json:"status"`
		AOID     string      `json:"aoid"`
		PayPrice json.Number `json:"pay_price"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("xorpay parse query: %w", err)
	}
	amount, _ := strconv.ParseFloat(resp.PayPrice.String(), 64)
	return &payment.QueryOrderResponse{
		TradeNo:  firstNonEmpty(resp.AOID, orderID),
		Status:   xorPayMapOrderStatus(resp.Status),
		Amount:   amount,
		Metadata: x.MerchantIdentityMetadata(),
	}, nil
}

func xorPayMapOrderStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case xorPayStatusPayed, xorPayStatusSuccess:
		return payment.ProviderStatusPaid
	case xorPayStatusNew:
		return payment.ProviderStatusPending
	case xorPayStatusExpire, xorPayStatusNotExist:
		return payment.ProviderStatusFailed
	default:
		return payment.ProviderStatusPending
	}
}

func (x *XorPay) VerifyNotification(_ context.Context, rawBody string, _ map[string]string) (*payment.PaymentNotification, error) {
	values, err := url.ParseQuery(rawBody)
	if err != nil {
		return nil, fmt.Errorf("parse notify: %w", err)
	}
	params := map[string]string{}
	for k := range values {
		params[k] = values.Get(k)
	}
	sign := strings.TrimSpace(params["sign"])
	if sign == "" {
		return nil, fmt.Errorf("missing sign")
	}
	want := xorPayMD5(params["aoid"] + params["order_id"] + params["pay_price"] + params["pay_time"] + x.config["appSecret"])
	if !hmac.Equal([]byte(strings.ToLower(sign)), []byte(want)) {
		return nil, fmt.Errorf("invalid signature")
	}
	amount, _ := strconv.ParseFloat(params["pay_price"], 64)
	metadata := x.MerchantIdentityMetadata()
	if metadata == nil {
		metadata = map[string]string{}
	}
	for _, k := range []string{"more", "detail", "pay_time"} {
		if v := strings.TrimSpace(params[k]); v != "" {
			metadata[k] = v
		}
	}
	return &payment.PaymentNotification{
		TradeNo:  params["aoid"],
		OrderID:  params["order_id"],
		Amount:   amount,
		Status:   payment.ProviderStatusSuccess,
		RawData:  rawBody,
		Metadata: metadata,
	}, nil
}

func (x *XorPay) Refund(ctx context.Context, req payment.RefundRequest) (*payment.RefundResponse, error) {
	aoid := strings.TrimSpace(req.TradeNo)
	if aoid == "" {
		return nil, fmt.Errorf("xorpay refund requires XorPay aoid/trade_no")
	}
	params := map[string]string{
		"price": req.Amount,
		"sign":  xorPayMD5(req.Amount + x.config["appSecret"]),
	}
	endpoint := fmt.Sprintf("%s/api/refund/%s", x.apiBase(), url.PathEscape(aoid))
	body, status, err := x.postForm(ctx, endpoint, params)
	if err != nil {
		return nil, fmt.Errorf("xorpay refund: %w", err)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("xorpay refund HTTP %d: %s", status, summarizeXorPayResponse(body))
	}
	var resp struct {
		Status string `json:"status"`
		Info   string `json:"info"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("xorpay parse refund: %w", err)
	}
	if resp.Status != xorPayStatusOK {
		return nil, fmt.Errorf("xorpay refund failed: %s %s", resp.Status, resp.Info)
	}
	return &payment.RefundResponse{RefundID: aoid, Status: payment.ProviderStatusSuccess}, nil
}

func (x *XorPay) postForm(ctx context.Context, endpoint string, params map[string]string) ([]byte, int, error) {
	form := url.Values{}
	for k, v := range params {
		if strings.TrimSpace(v) != "" {
			form.Set(k, v)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return x.do(req)
}

func (x *XorPay) get(ctx context.Context, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	return x.do(req)
}

func (x *XorPay) do(req *http.Request) ([]byte, int, error) {
	client := x.httpClient
	if client == nil {
		client = &http.Client{Timeout: xorPayHTTPTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxXorPayResponseSize))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func xorPayCreateSign(name, payType, price, orderID, notifyURL, appSecret string) string {
	return xorPayMD5(name + payType + price + orderID + notifyURL + appSecret)
}

func xorPayMD5(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func summarizeXorPayResponse(body []byte) string {
	summary := strings.Join(strings.Fields(string(body)), " ")
	if summary == "" {
		return "<empty>"
	}
	if len(summary) > maxXorPayErrorSummary {
		return summary[:maxXorPayErrorSummary] + "..."
	}
	return summary
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
