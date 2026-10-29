package main

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxLakeDeleteObjects   = 1000
	maxLakeDeleteBodyBytes = 8 * 1024 * 1024
	s3XMLNamespace         = "http://s3.amazonaws.com/doc/2006-03-01/"
)

type lakeDeleteRequest struct {
	Keys  []string
	Quiet bool
}

type s3DeletedObject struct {
	Key string `xml:"Key"`
}

type s3DeleteError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

type s3DeleteResult struct {
	XMLName xml.Name          `xml:"DeleteResult"`
	XMLNS   string            `xml:"xmlns,attr"`
	Deleted []s3DeletedObject `xml:"Deleted,omitempty"`
	Errors  []s3DeleteError   `xml:"Error,omitempty"`
}

func isLakeDeleteObjectsRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Query().Has("delete")
}

func parseLakeDeleteBucket(r *http.Request) (string, error) {
	path, err := url.PathUnescape(r.URL.EscapedPath())
	if err != nil || !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("invalid path")
	}
	bucket := strings.TrimPrefix(path, "/")
	bucket = strings.TrimSuffix(bucket, "/")
	if bucket == "" || strings.Contains(bucket, "/") ||
		(!kubernetesName.MatchString(bucket) && !lakeBucketPattern.MatchString(bucket)) {
		return "", fmt.Errorf("invalid bucket")
	}
	return bucket, nil
}

func (s *server) handleLakeDeleteObjects(
	w http.ResponseWriter,
	r *http.Request,
	tenant, account string,
	scope StorageScope,
	bucket string,
) {
	if !s.authorizeLake(w, tenant, account, OperationLakeDelete) {
		return
	}
	query := r.URL.Query()
	if len(query) != 1 || len(query["delete"]) != 1 || query["delete"][0] != "" {
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest", "Only the delete subresource is supported")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/xml") {
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest", "Content-Type must be application/xml")
		return
	}
	md5Values := r.Header.Values("Content-MD5")
	if len(md5Values) != 1 {
		writeS3Error(w, http.StatusBadRequest, "InvalidDigest", "Content-MD5 is required")
		return
	}
	declaredMD5, err := base64.StdEncoding.Strict().DecodeString(md5Values[0])
	if err != nil || len(declaredMD5) != md5.Size {
		writeS3Error(w, http.StatusBadRequest, "InvalidDigest", "Content-MD5 is invalid")
		return
	}
	if r.ContentLength > maxLakeDeleteBodyBytes {
		writeS3Error(w, http.StatusRequestEntityTooLarge, "EntityTooLarge", "Delete request body is too large")
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLakeDeleteBodyBytes))
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeS3Error(w, http.StatusRequestEntityTooLarge, "EntityTooLarge", "Delete request body is too large")
			return
		}
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "The XML request body is malformed")
		return
	}
	observedMD5 := md5.Sum(payload)
	if subtle.ConstantTimeCompare(declaredMD5, observedMD5[:]) != 1 {
		writeS3Error(w, http.StatusBadRequest, "BadDigest", "Content-MD5 does not match the request body")
		return
	}
	request, err := parseLakeDeleteXML(payload, s.cfg.DuckLakeRoot)
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "The XML request body is malformed")
		return
	}
	unlock, err := s.governedStore.AcquireLakePathLocks(r.Context(), tenant, bucket, request.Keys)
	if err != nil {
		s.writeLakeStoreError(w, err)
		return
	}
	defer unlock()
	objects, err := s.governedStore.DeleteLakeObjects(r.Context(), tenant, scope, bucket, request.Keys, time.Now())
	if err != nil {
		s.writeLakeReadError(w, err)
		return
	}
	if len(objects) != len(request.Keys) {
		s.log.Error("lake batch delete returned incomplete authorization", "tenant", tenant)
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "Governed storage unavailable")
		return
	}
	outcomes, err := s.lakeBackend.DeleteObjects(r.Context(), bucket, request.Keys)
	if err != nil {
		s.log.Error("lake physical batch delete failed", "tenant", tenant, "err", err.Error())
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Object storage delete failed")
		return
	}
	for index := range outcomes {
		if !outcomes[index].Deleted {
			continue
		}
		key := outcomes[index].Key
		if err := s.lakeBackend.ConfirmAbsent(r.Context(), bucket, key); err != nil {
			outcomes[index] = lakeDeleteOutcome{Key: key, Code: "InternalError",
				Message: "Object storage delete remains unconfirmed"}
			continue
		}
		if err := s.governedStore.ConfirmLakeObjectRemoval(r.Context(), tenant, scope, bucket, key,
			time.Now()); err != nil {
			outcomes[index] = lakeDeleteOutcome{Key: key, Code: "InternalError",
				Message: "Governed deletion confirmation failed"}
		}
	}
	result, ok := buildLakeDeleteResult(request, outcomes)
	if !ok {
		s.log.Error("lake physical batch delete returned an invalid result", "tenant", tenant)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Object storage returned an invalid delete result")
		return
	}
	writeS3XML(w, http.StatusOK, result)
}

func buildLakeDeleteResult(request lakeDeleteRequest, outcomes []lakeDeleteOutcome) (s3DeleteResult, bool) {
	requested := make(map[string]struct{}, len(request.Keys))
	for _, key := range request.Keys {
		requested[key] = struct{}{}
	}
	byKey := make(map[string]lakeDeleteOutcome, len(outcomes))
	for _, outcome := range outcomes {
		if _, ok := requested[outcome.Key]; !ok {
			return s3DeleteResult{}, false
		}
		if _, duplicate := byKey[outcome.Key]; duplicate {
			return s3DeleteResult{}, false
		}
		if outcome.Deleted == (outcome.Code != "") {
			return s3DeleteResult{}, false
		}
		byKey[outcome.Key] = outcome
	}
	result := s3DeleteResult{XMLNS: s3XMLNamespace}
	for _, key := range request.Keys {
		outcome, ok := byKey[key]
		if !ok {
			result.Errors = append(result.Errors, s3DeleteError{
				Key: key, Code: "InternalError", Message: "Object storage did not confirm deletion",
			})
			continue
		}
		if outcome.Deleted {
			if !request.Quiet {
				result.Deleted = append(result.Deleted, s3DeletedObject{Key: key})
			}
			continue
		}
		message := outcome.Message
		if message == "" {
			message = "Object storage delete failed"
		}
		result.Errors = append(result.Errors, s3DeleteError{Key: key, Code: outcome.Code, Message: message})
	}
	return result, true
}

func parseLakeDeleteXML(payload []byte, root string) (lakeDeleteRequest, error) {
	decoder := xml.NewDecoder(strings.NewReader(string(payload)))
	decoder.Strict = true
	token, err := nextLakeXMLToken(decoder, true)
	if err != nil {
		return lakeDeleteRequest{}, err
	}
	start, ok := token.(xml.StartElement)
	if !ok || start.Name.Local != "Delete" || (start.Name.Space != "" && start.Name.Space != s3XMLNamespace) ||
		!validLakeXMLAttributes(start.Attr, true) {
		return lakeDeleteRequest{}, fmt.Errorf("invalid delete root")
	}
	namespace := start.Name.Space
	request := lakeDeleteRequest{}
	quietSeen := false
	seenKeys := make(map[string]struct{})
	for {
		token, err = decoder.Token()
		if err != nil {
			return lakeDeleteRequest{}, err
		}
		switch value := token.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return lakeDeleteRequest{}, fmt.Errorf("unexpected delete text")
			}
		case xml.StartElement:
			if value.Name.Space != namespace || !validLakeXMLAttributes(value.Attr, false) {
				return lakeDeleteRequest{}, fmt.Errorf("invalid delete element")
			}
			switch value.Name.Local {
			case "Object":
				key, parseErr := parseLakeDeleteObject(decoder, value, namespace)
				if parseErr != nil || validKey(key) != nil || !strings.HasPrefix(key, root) {
					return lakeDeleteRequest{}, fmt.Errorf("invalid delete object")
				}
				if _, duplicate := seenKeys[key]; duplicate {
					return lakeDeleteRequest{}, fmt.Errorf("duplicate delete key")
				}
				seenKeys[key] = struct{}{}
				request.Keys = append(request.Keys, key)
				if len(request.Keys) > maxLakeDeleteObjects {
					return lakeDeleteRequest{}, fmt.Errorf("too many delete keys")
				}
			case "Quiet":
				if quietSeen {
					return lakeDeleteRequest{}, fmt.Errorf("duplicate quiet element")
				}
				quietSeen = true
				text, parseErr := parseLakeXMLText(decoder, value)
				if parseErr != nil {
					return lakeDeleteRequest{}, parseErr
				}
				switch strings.TrimSpace(text) {
				case "true", "1":
					request.Quiet = true
				case "false", "0":
					request.Quiet = false
				default:
					return lakeDeleteRequest{}, fmt.Errorf("invalid quiet value")
				}
			default:
				return lakeDeleteRequest{}, fmt.Errorf("unsupported delete element")
			}
		case xml.EndElement:
			if value.Name != start.Name {
				return lakeDeleteRequest{}, fmt.Errorf("invalid delete end")
			}
			if len(request.Keys) == 0 {
				return lakeDeleteRequest{}, fmt.Errorf("empty delete request")
			}
			if _, err := nextLakeXMLToken(decoder, false); !errors.Is(err, io.EOF) {
				return lakeDeleteRequest{}, fmt.Errorf("trailing XML data")
			}
			return request, nil
		default:
			return lakeDeleteRequest{}, fmt.Errorf("unsupported XML token")
		}
	}
}

func parseLakeDeleteObject(decoder *xml.Decoder, start xml.StartElement, namespace string) (string, error) {
	keySeen := false
	key := ""
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		switch value := token.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return "", fmt.Errorf("unexpected object text")
			}
		case xml.StartElement:
			if value.Name.Space != namespace || !validLakeXMLAttributes(value.Attr, false) {
				return "", fmt.Errorf("invalid object element")
			}
			if value.Name.Local == "VersionId" {
				return "", fmt.Errorf("version IDs are unsupported")
			}
			if value.Name.Local != "Key" || keySeen {
				return "", fmt.Errorf("invalid key representation")
			}
			keySeen = true
			key, err = parseLakeXMLText(decoder, value)
			if err != nil {
				return "", err
			}
		case xml.EndElement:
			if value.Name != start.Name || !keySeen {
				return "", fmt.Errorf("invalid object end")
			}
			return key, nil
		default:
			return "", fmt.Errorf("unsupported XML token")
		}
	}
}

func parseLakeXMLText(decoder *xml.Decoder, start xml.StartElement) (string, error) {
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		switch value := token.(type) {
		case xml.CharData:
			text.Write(value)
		case xml.EndElement:
			if value.Name != start.Name {
				return "", fmt.Errorf("invalid text element end")
			}
			return text.String(), nil
		default:
			return "", fmt.Errorf("invalid text representation")
		}
	}
}

func nextLakeXMLToken(decoder *xml.Decoder, allowDeclaration bool) (xml.Token, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(value)) == "" {
				continue
			}
		case xml.ProcInst:
			if allowDeclaration && value.Target == "xml" {
				allowDeclaration = false
				continue
			}
		}
		return token, nil
	}
}

func validLakeXMLAttributes(attributes []xml.Attr, root bool) bool {
	if len(attributes) == 0 {
		return true
	}
	return root && len(attributes) == 1 && attributes[0].Name.Space == "" &&
		attributes[0].Name.Local == "xmlns" && attributes[0].Value == s3XMLNamespace
}

func writeS3XML(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(body)
}
