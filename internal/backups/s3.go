package backups

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// Minimal S3-compatible PUT using AWS Signature Version 4 with only the
// standard library - no SDK dependency. Streams the file from disk.
func UploadS3(ctx context.Context, endpoint, region, bucket, key, accessKey, secretKey, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	// Payload hash requires a pass over the file; stream it with a tee in a
	// real deployment this is done by the agent. Here we hash first, then PUT.
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	payloadHash := hex.EncodeToString(h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	endpoint = strings.TrimSuffix(endpoint, "/")
	url := fmt.Sprintf("%s/%s/%s", endpoint, bucket, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size()
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Content-Type", "application/gzip")

	host := req.URL.Host
	signedHeaders := []string{"content-type", "host", "x-amz-content-sha256", "x-amz-date"}
	sort.Strings(signedHeaders)
	var headerVals []string
	for _, hn := range signedHeaders {
		var v string
		switch hn {
		case "host":
			v = host
		default:
			v = req.Header.Get(hn)
		}
		headerVals = append(headerVals, hn+":"+strings.TrimSpace(v))
	}
	canonicalRequest := strings.Join([]string{
		http.MethodPut, req.URL.EscapedPath(), "", strings.Join(headerVals, "\n") + "\n",
		strings.Join(signedHeaders, ";"), payloadHash,
	}, "\n")
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, regionOrDefault(region))
	crh := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(crh[:])
	signingKey := deriveKey(secretKey, dateStamp, regionOrDefault(region))
	sig := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, strings.Join(signedHeaders, ";"), sig))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("S3 PUT failed: %s: %s", resp.Status, string(body))
	}
	return nil
}

func regionOrDefault(r string) string {
	if r == "" {
		return "us-east-1"
	}
	return r
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func deriveKey(secret, date, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	return hmacSHA256(kService, "aws4_request")
}
