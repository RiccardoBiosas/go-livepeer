package drivers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/clog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/net"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

const (
	// S3_POLICY_EXPIRE_IN_HOURS how long access rights given to other node will be valid
	S3_POLICY_EXPIRE_IN_HOURS = 24
	// defaultSaveTimeout is used on save ops when no custom timeout is provided.
	defaultSaveTimeout = 10 * time.Second
	// uploaderConcurrency controls how many parts to upload in parallel when
	// saving a file to S3. Will only make a difference for large files (not small
	// video segments), since we use a big part size.
	uploaderConcurrency = 8
	// uploderPartSize is the size of the parts that will be uploaded to the
	// S3-compatible service. Fine-tuned for Storj (chunk size of 64MB) and Google
	// Cloud Storage (also improves performance). Can make this configurable in
	// the future for optimized support of other storage providers.
	uploaderPartSize = 63 * 1024 * 1024
)

/* S3OS S3 backed object storage driver. For own storage access key and access key secret
   should be specified. To give to other nodes access to own S3 storage so called 'POST' policy
   is created. This policy is valid for S3_POLICY_EXPIRE_IN_HOURS hours.
*/
type s3OS struct {
	host               string
	region             string
	bucket             string
	awsAccessKeyID     string
	awsSecretAccessKey string
	s3svc              *s3.S3
	s3sess             *session.Session
	useFullAPI         bool
}

type s3Session struct {
	os          *s3OS
	host        string
	bucket      string
	key         string
	policy      string
	signature   string
	credential  string
	xAmzDate    string
	storageType net.OSInfo_StorageType
	fields      map[string]string
	s3svc       *s3.S3
	s3sess      *session.Session
}

func s3Host(bucket string) string {
	return fmt.Sprintf("https://%s.s3.amazonaws.com", bucket)
}

func newS3Session(info *net.S3OSInfo) OSSession {
	sess := &s3Session{
		host:        info.Host,
		key:         info.Key,
		policy:      info.Policy,
		signature:   info.Signature,
		xAmzDate:    info.XAmzDate,
		credential:  info.Credential,
		storageType: net.OSInfo_S3,
	}
	sess.fields = s3GetFields(sess)
	return sess
}

func NewS3Driver(region, bucket, accessKey, accessKeySecret string, useFullAPI bool) (OSDriver, error) {
	glog.Infof("Creating S3 with region %s bucket %s", region, bucket)
	os := &s3OS{
		host:               s3Host(bucket),
		region:             region,
		bucket:             bucket,
		awsAccessKeyID:     accessKey,
		awsSecretAccessKey: accessKeySecret,
		useFullAPI:         useFullAPI,
	}
	if os.awsAccessKeyID != "" {
		var err error
		creds := credentials.NewStaticCredentials(os.awsAccessKeyID, os.awsSecretAccessKey, "")
		cfg := aws.NewConfig().
			WithRegion(os.region).
			WithCredentials(creds)
		os.s3sess, err = session.NewSession(cfg)
		if err != nil {
			return nil, err
		}
		os.s3svc = s3.New(os.s3sess)
	}
	return os, nil
}

// NewCustomS3Driver for creating S3-compatible stores other than S3 itself
func NewCustomS3Driver(host, bucket, region, accessKey, accessKeySecret string, useFullAPI bool) (OSDriver, error) {
	glog.Infof("using custom s3 with url: %s, bucket %s region %s use full API %v", host, bucket, region, useFullAPI)
	os := &s3OS{
		host:               host,
		bucket:             bucket,
		awsAccessKeyID:     accessKey,
		awsSecretAccessKey: accessKeySecret,
		region:             region,
		useFullAPI:         useFullAPI,
	}
	if !useFullAPI {
		os.host += "/" + bucket
	}
	if os.awsAccessKeyID != "" {
		var err error
		creds := credentials.NewStaticCredentials(os.awsAccessKeyID, os.awsSecretAccessKey, "")
		cfg := aws.NewConfig().
			WithRegion(os.region).
			WithCredentials(creds).
			WithEndpoint(host).
			WithS3ForcePathStyle(true)
		os.s3sess, err = session.NewSession(cfg)
		if err != nil {
			return nil, err
		}
		os.s3svc = s3.New(os.s3sess)
	}
	return os, nil
}

func (os *s3OS) NewSession(path string) OSSession {
	policy, signature, credential, xAmzDate := createPolicy(os.awsAccessKeyID,
		os.bucket, os.region, os.awsSecretAccessKey, path)
	sess := &s3Session{
		os:          os,
		host:        os.host,
		bucket:      os.bucket,
		key:         path,
		policy:      policy,
		signature:   signature,
		credential:  credential,
		xAmzDate:    xAmzDate,
		storageType: net.OSInfo_S3,
	}
	if os.useFullAPI {
		sess.s3svc = os.s3svc
		sess.s3sess = os.s3sess
	}
	sess.fields = s3GetFields(sess)
	return sess
}

func s3GetFields(sess *s3Session) map[string]string {
	return map[string]string{
		"x-amz-algorithm":  "AWS4-HMAC-SHA256",
		"x-amz-credential": sess.credential,
		"x-amz-date":       sess.xAmzDate,
		"x-amz-signature":  sess.signature,
	}
}

func (os *s3Session) OS() OSDriver {
	return os.os
}

func (os *s3Session) IsExternal() bool {
	return true
}

func (os *s3Session) EndSession() {
}

type s3pageInfo struct {
	files       []FileInfo
	directories []string
	ctx         context.Context
	s3svc       *s3.S3
	params      *s3.ListObjectsInput
	nextMarker  string
}

func (s3pi *s3pageInfo) Files() []FileInfo {
	return s3pi.files
}
func (s3pi *s3pageInfo) Directories() []string {
	return s3pi.directories
}
func (s3pi *s3pageInfo) HasNextPage() bool {
	return s3pi.nextMarker != ""
}
func (s3pi *s3pageInfo) NextPage() (PageInfo, error) {
	if s3pi.nextMarker == "" {
		return nil, ErrNoNextPage
	}
	next := &s3pageInfo{
		s3svc:  s3pi.s3svc,
		params: s3pi.params,
		ctx:    s3pi.ctx,
	}
	next.params.Marker = &s3pi.nextMarker
	if err := next.listFiles(); err != nil {
		return nil, err
	}
	return next, nil
}

func (s3pi *s3pageInfo) listFiles() error {
	resp, err := s3pi.s3svc.ListObjectsWithContext(s3pi.ctx, s3pi.params)
	if err != nil {
		return err
	}
	for _, cont := range resp.CommonPrefixes {
		s3pi.directories = append(s3pi.directories, *cont.Prefix)
	}
	for _, cont := range resp.Contents {
		fi := FileInfo{
			Name:         *cont.Key,
			ETag:         *cont.ETag,
			LastModified: *cont.LastModified,
			Size:         cont.Size,
		}
		s3pi.files = append(s3pi.files, fi)
	}
	if resp.NextMarker != nil {
		s3pi.nextMarker = *resp.NextMarker
	} else if *resp.IsTruncated && len(resp.Contents) > 0 {
		s3pi.nextMarker = *resp.Contents[len(resp.Contents)-1].Key
	}
	return nil
}

func (os *s3Session) ListFiles(ctx context.Context, prefix, delim string) (PageInfo, error) {
	if os.s3svc != nil {
		bucket := aws.String(os.bucket)
		params := &s3.ListObjectsInput{
			Bucket: bucket,
		}
		if prefix != "" {
			params.Prefix = aws.String(prefix)
		}
		if delim != "" {
			params.Delimiter = aws.String(delim)
		}
		pi := &s3pageInfo{
			ctx:    ctx,
			s3svc:  os.s3svc,
			params: params,
		}
		if err := pi.listFiles(); err != nil {
			return nil, err
		}
		return pi, nil
	}

	return nil, fmt.Errorf("Not implemented")
}

func (os *s3Session) ReadData(ctx context.Context, name string) (*FileInfoReader, error) {
	if os.s3svc == nil {
		return nil, fmt.Errorf("Not implemented")
	}
	params := &s3.GetObjectInput{
		Bucket: aws.String(os.bucket),
		Key:    aws.String(name),
	}
	resp, err := os.s3svc.GetObjectWithContext(ctx, params)
	if err != nil {
		return nil, err
	}
	res := &FileInfoReader{
		Body: resp.Body,
	}
	res.LastModified = *resp.LastModified
	res.ETag = *resp.ETag
	res.Name = name
	res.Size = resp.ContentLength
	if len(resp.Metadata) > 0 {
		res.Metadata = make(map[string]string, len(resp.Metadata))
		for k, v := range resp.Metadata {
			res.Metadata[k] = *v
		}
	}
	return res, nil
}

func (os *s3Session) saveDataPut(ctx context.Context, name string, data io.Reader, meta map[string]string, timeout time.Duration) (string, error) {
	now := time.Now()
	bucket := aws.String(os.bucket)
	keyname := aws.String(os.key + "/" + name)
	var metadata map[string]*string
	if len(meta) > 0 {
		metadata = make(map[string]*string)
		for k, v := range meta {
			metadata[k] = aws.String(v)
		}
	}
	data, contentType, err := os.peekContentType(name, data)
	if err != nil {
		return "", err
	}

	uploader := s3manager.NewUploader(os.s3sess, func(u *s3manager.Uploader) {
		u.Concurrency = uploaderConcurrency
		u.PartSize = uploaderPartSize
		u.RequestOptions = append(u.RequestOptions, request.WithLogLevel(aws.LogDebug))
	})
	params := &s3manager.UploadInput{
		Bucket:      bucket,
		Key:         keyname,
		Metadata:    metadata,
		Body:        data,
		ContentType: aws.String(contentType),
	}
	if timeout == 0 {
		timeout = defaultSaveTimeout
	}
	ctx, cancel := context.WithTimeout(clog.Clone(context.Background(), ctx), timeout)
	resp, err := uploader.UploadWithContext(ctx, params)
	cancel()
	if err != nil {
		return "", err
	}

	clog.Infof(ctx, "resp: %+v", resp)
	url := os.getAbsURL(*keyname)
	clog.V(common.VERBOSE).Infof(ctx, "Saved to S3 url=%s dur=%s", url, time.Since(now))
	return url, nil
}

func (os *s3Session) SaveData(ctx context.Context, name string, data io.Reader, meta map[string]string, timeout time.Duration) (string, error) {
	if os.s3svc != nil {
		return os.saveDataPut(ctx, name, data, meta, timeout)
	}
	// tentativeUrl just used for logging
	tentativeURL := path.Join(os.host, os.key, name)
	clog.V(common.VERBOSE).Infof(ctx, "Saving to S3 url=%s", tentativeURL)
	started := time.Now()
	path, err := os.postData(ctx, name, data, meta, timeout)
	if err != nil {
		// handle error
		clog.Errorf(ctx, "Save S3 error err=%q", err)
		return "", err
	}

	url := os.getAbsURL(path)
	clog.V(common.VERBOSE).Infof(ctx, "Saved to S3 url=%s dur=%s", tentativeURL, time.Since(started))
	return url, nil
}

func (os *s3Session) getAbsURL(path string) string {
	if strings.Contains(os.host, os.bucket) {
		return os.host + "/" + path
	}
	return os.host + "/" + os.bucket + "/" + path
}

func (os *s3Session) GetInfo() *net.OSInfo {
	oi := &net.OSInfo{
		S3Info: &net.S3OSInfo{
			Host:       os.host,
			Key:        os.key,
			Policy:     os.policy,
			Signature:  os.signature,
			Credential: os.credential,
			XAmzDate:   os.xAmzDate,
		},
		StorageType: os.storageType,
	}
	return oi
}

func (os *s3Session) peekContentType(fileName string, data io.Reader) (*bufio.Reader, string, error) {
	bufData := bufio.NewReaderSize(data, 4096)
	firstBytes, err := bufData.Peek(512)
	if err != nil && err != io.EOF {
		return nil, "", err
	}
	ext := path.Ext(fileName)
	fileType, err := common.TypeByExtension(ext)
	if err != nil {
		fileType = http.DetectContentType(firstBytes)
	}
	return bufData, fileType, nil
}

// if s3 storage is not our own, we are saving data into it using POST request
func (os *s3Session) postData(ctx context.Context, fileName string, data io.Reader, meta map[string]string, timeout time.Duration) (string, error) {
	data, fileType, err := os.peekContentType(fileName, data)
	if err != nil {
		return "", err
	}
	path, fileName := path.Split(path.Join(os.key, fileName))
	fields := map[string]string{
		"acl":          "public-read",
		"Content-Type": fileType,
		"key":          path + "${filename}",
		"policy":       os.policy,
	}
	for k, v := range os.fields {
		fields[k] = v
	}
	postURL := os.host
	if !strings.Contains(postURL, os.bucket) {
		postURL += "/" + os.bucket
	}
	req, cancel, err := newfileUploadRequest(ctx, postURL, fields, data, fileName, timeout)
	if err != nil {
		clog.Errorf(ctx, "err=%q", err)
		return "", err
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		clog.Errorf(ctx, "Error saving data to S3 err=%q", err)
		return "", err
	}
	cancel()
	body := &bytes.Buffer{}
	sz, err := body.ReadFrom(resp.Body)
	if err != nil {
		clog.Errorf(ctx, "err=%q", err)
		return "", err
	}
	resp.Body.Close()
	if sz > 0 {
		// usually there's an error at this point, so log
		clog.Errorf(ctx, "Got response from from S3 err=%s", body)
		return "", fmt.Errorf(body.String()) // sorta bad
	}
	return path + fileName, err
}

func (os *s3Session) IsOwn(url string) bool {
	return strings.HasPrefix(url, os.host)
}

func makeHmac(key []byte, data []byte) []byte {
	hash := hmac.New(sha256.New, key)
	hash.Write(data)
	return hash.Sum(nil)
}

func signString(stringToSign, sregion, amzDate, secret string) string {
	date := makeHmac([]byte("AWS4"+secret), []byte(amzDate))
	region := makeHmac(date, []byte(sregion))
	service := makeHmac(region, []byte("s3"))
	credentials := makeHmac(service, []byte("aws4_request"))
	signature := makeHmac(credentials, []byte(stringToSign))
	sSignature := hex.EncodeToString(signature)
	return sSignature
}

// createPolicy returns policy, signature, xAmzCredentail and xAmzDate
func createPolicy(key, bucket, region, secret, path string) (string, string, string, string) {
	const timeFormat = "2006-01-02T15:04:05.999Z"
	const shortTimeFormat = "20060102"

	expireAt := time.Now().Add(S3_POLICY_EXPIRE_IN_HOURS * time.Hour)
	expireFmt := expireAt.UTC().Format(timeFormat)
	xAmzDate := time.Now().UTC().Format(shortTimeFormat)
	xAmzCredential := fmt.Sprintf("%s/%s/%s/s3/aws4_request", key, xAmzDate, region)
	src := fmt.Sprintf(`{ "expiration": "%s",
    "conditions": [
      {"bucket": "%s"},
      {"acl": "public-read"},
      ["starts-with", "$Content-Type", ""],
      ["starts-with", "$key", "%s"],
      {"x-amz-algorithm": "AWS4-HMAC-SHA256"},
      {"x-amz-credential": "%s"},
      {"x-amz-date": "%sT000000Z" }
    ]
  }`, expireFmt, bucket, path, xAmzCredential, xAmzDate)
	policy := base64.StdEncoding.EncodeToString([]byte(src))
	return policy, signString(policy, region, xAmzDate, secret), xAmzCredential, xAmzDate + "T000000Z"
}

func newfileUploadRequest(ctx context.Context, uri string, params map[string]string, fData io.Reader, fileName string, timeout time.Duration) (*http.Request, context.CancelFunc, error) {
	clog.Infof(ctx, "Posting data to %s (params %+v)", uri, params)
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for key, val := range params {
		err := writer.WriteField(key, val)
		if err != nil {
			clog.Errorf(ctx, "err=%q", err)
		}
	}
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return nil, nil, err
	}
	_, err = io.Copy(part, fData)

	err = writer.Close()
	if err != nil {
		return nil, nil, err
	}
	if timeout == 0 {
		timeout = defaultSaveTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req, err := http.NewRequestWithContext(ctx, "POST", uri, body)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req, cancel, err
}
