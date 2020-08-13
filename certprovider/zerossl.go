package certprovider

import (
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
)

var (
	zeroSSLBaseUrl = "https://api.zerossl.com"
)

type ZeroSSLError struct {
	Code string `json:"code"`
	Type string `json:"type"`
}

type ZeroSSLApiError struct {
	Success string       `json:"success"`
	Error   ZeroSSLError `json:"error"`
}

type ZeroSSLValidationMethod struct {
	FileValidationUrlHttp  string   `json:"file_validation_url_http"`
	FileValidationUrlHttps string   `json:"file_validation_url_https"`
	FileValidationContent  []string `json:"file_validation_content"`
	CnameValidationP1      string   `json:"cname_validation_p1"`
	CnameValidationP2      string   `json:"cname_validation_p2"`
}

type ZeroSSLValidation struct {
	EmailValidation map[string][]string                `json:"email_validation"`
	OtherValidation map[string]ZeroSSLValidationMethod `json:"other_methods"`
}

type ZeroSSLExternalCert struct {
	Id                string            `json:"id"`
	Type              string            `json:"type"`
	CommonName        string            `json:"common_name"`
	AdditionalDomains string            `json:"additional_domains"`
	Created           string            `json:"created"`
	Expires           string            `json:"expires"`
	Status            string            `json:"status"`
	ValidationType    string            `json:"validation_type"`
	ValidationEmails  string            `json:"validation_emails"`
	ReplacementFor    string            `json:"replacement_for"`
	Validation        ZeroSSLValidation `json:"validation"`
}

type ZeroSSLCertResponse struct {
	Certificate string `json:"certificate.crt"`
	CaBundle    string `json:"ca_bundle.crt"`
}

func ZeroSSLGenerateCsr(keyBytes []byte, domain string) (csrBuffer bytes.Buffer, err error) {
	block, _ := pem.Decode(keyBytes)
	x509Encoded := block.Bytes
	privKey, err := x509.ParsePKCS1PrivateKey(x509Encoded)
	if err != nil {
		return csrBuffer, err
	}
	subj := pkix.Name{
		CommonName: domain,
	}
	rawSubj := subj.ToRDNSequence()
	asn1Subj, _ := asn1.Marshal(rawSubj)
	template := x509.CertificateRequest{
		RawSubject:         asn1Subj,
		SignatureAlgorithm: x509.SHA256WithRSA,
	}
	csrBytes, err := x509.CreateCertificateRequest(rand.Reader, &template, privKey)
	if err != nil {
		return csrBuffer, err
	}
	pem.Encode(&csrBuffer, &pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrBytes})
	return csrBuffer, nil
}

func ZeroSSLRequestCert(csr bytes.Buffer, domain string) (certificate ZeroSSLExternalCert, err error) {
	apiKey, found := os.LookupEnv("ZEROSSL_API_KEY")
	if !found {
		return certificate, fmt.Errorf("Failed to get the ZEROSSL_API_KEY environment variable. Make sure it's set")
	}
	parsedCsr := strings.Replace(csr.String(), "\n", "", -1)
	data := url.Values{}
	data.Set("certificate_domains", domain)
	data.Set("certificate_validity_days", "90")
	data.Set("certificate_csr", parsedCsr)
	apiUrl := fmt.Sprintf(
		"%s/certificates?access_key=%s",
		zeroSSLBaseUrl, apiKey,
	)
	client := &http.Client{}
	request, err := http.NewRequest("POST", apiUrl, strings.NewReader(data.Encode()))
	if err != nil {
		return certificate, err
	}
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(request)
	if err != nil {
		return certificate, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return certificate, fmt.Errorf("Received bad response from ZeroSSL: %v - %v", resp.StatusCode, string(body))
	}
	body, _ := ioutil.ReadAll(resp.Body)
	err = json.Unmarshal(body, &certificate)
	if err != nil || certificate.Id == "" {
		var apiError ZeroSSLApiError
		err = json.Unmarshal(body, &apiError)
		if err != nil {
			return certificate, fmt.Errorf("Unknown error occured: %v", string(body))
		}
		return certificate, fmt.Errorf("There was a problem requesting a certificate: %v", apiError.Error.Type)
	}
	return certificate, nil
}

func ZeroSSLValidateCert(certificate ZeroSSLExternalCert) error {
	apiKey, found := os.LookupEnv("ZEROSSL_API_KEY")
	if !found {
		return fmt.Errorf("Failed to get the ZEROSSL_API_KEY environment variable. Make sure it's set")
	}
	apiUrl := fmt.Sprintf(
		"%s/certificates/%s/challenges?access_key=%s",
		zeroSSLBaseUrl, certificate.Id, apiKey,
	)
	data := url.Values{}
	data.Set("validation_method", "HTTP_CSR_HASH")
	client := &http.Client{}
	request, err := http.NewRequest("POST", apiUrl, strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("Received bad response from ZeroSSL: %v - %v", resp.StatusCode, string(body))
	}
	return nil
}

func ZeroSSLGetCert(certificate ZeroSSLExternalCert) (newCertificate ZeroSSLExternalCert, err error) {
	apiKey, found := os.LookupEnv("ZEROSSL_API_KEY")
	if !found {
		return newCertificate, fmt.Errorf("Failed to get the ZEROSSL_API_KEY environment variable. Make sure it's set")
	}
	apiUrl := fmt.Sprintf(
		"%s/certificates/%s?access_key=%s",
		zeroSSLBaseUrl, certificate.Id, apiKey,
	)
	client := &http.Client{}
	request, err := http.NewRequest("GET", apiUrl, nil)
	if err != nil {
		return newCertificate, err
	}
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(request)
	if err != nil {
		return newCertificate, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return newCertificate, fmt.Errorf("Received bad response from ZeroSSL: %v - %v", resp.StatusCode, string(body))
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return newCertificate, err
	}
	err = json.Unmarshal(body, &newCertificate)
	if err != nil {
		var apiError ZeroSSLApiError
		err = json.Unmarshal(body, &apiError)
		if err != nil {
			fmt.Printf("Unknown error occured: %v\n", string(body))
		}
		fmt.Printf("There was a problem requesting a certificate: %s", apiError.Error.Type)
	}
	return newCertificate, nil
}

func ZeroSSLDownloadCert(certificate ZeroSSLExternalCert) (string, string, error) {
	apiKey, found := os.LookupEnv("ZEROSSL_API_KEY")
	if !found {
		return "", "", fmt.Errorf("Failed to get the ZEROSSL_API_KEY environment variable. Make sure it's set")
	}
	apiUrl := fmt.Sprintf(
		"%s/certificates/%s/download/return?access_key=%s",
		zeroSSLBaseUrl, certificate.Id, apiKey,
	)
	client := &http.Client{}
	request, err := http.NewRequest("GET", apiUrl, nil)
	if err != nil {
		return "", "", err
	}
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(request)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return "", "", fmt.Errorf("Received bad response from ZeroSSL: %v - %v", resp.StatusCode, string(body))
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	var certResponse ZeroSSLCertResponse
	err = json.Unmarshal(body, &certResponse)
	if err != nil {
		var apiError ZeroSSLApiError
		err = json.Unmarshal(body, &apiError)
		if err != nil {
			return "", "", fmt.Errorf("Unknown error occured: %v", string(body))
		}
		return "", "", fmt.Errorf("There was a problem requesting a certificate: %s", apiError.Error.Type)
	}
	return certResponse.Certificate, certResponse.CaBundle, nil
}
