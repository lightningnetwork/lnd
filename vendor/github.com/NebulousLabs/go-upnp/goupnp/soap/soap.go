// Definition for the SOAP structure required for UPnP's SOAP usage.

package soap

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"reflect"
)

const (
	soapEncodingStyle = "http://schemas.xmlsoap.org/soap/encoding/"
	soapPrefix        = xml.Header + `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`
	soapSuffix        = `</s:Body></s:Envelope>`
)

type SOAPClient struct {
	EndpointURL url.URL
	HTTPClient  http.Client
}

func NewSOAPClient(endpointURL url.URL) *SOAPClient {
	return &SOAPClient{
		EndpointURL: endpointURL,
	}
}

// PerformSOAPAction makes a SOAP request, with the given action.
// inAction and outAction must both be pointers to structs with string fields
// only.
func (client *SOAPClient) PerformAction(actionNamespace, actionName string, inAction interface{}, outAction interface{}) error {
	requestBytes, err := encodeRequestAction(actionNamespace, actionName, inAction)
	if err != nil {
		return err
	}

	response, err := client.HTTPClient.Do(&http.Request{
		Method: "POST",
		URL:    &client.EndpointURL,
		Header: http.Header{
			"SOAPACTION":   []string{`"` + actionNamespace + "#" + actionName + `"`},
			"CONTENT-TYPE": []string{"text/xml; charset=\"utf-8\""},
		},
		Body: ioutil.NopCloser(bytes.NewBuffer(requestBytes)),
		// Set ContentLength to avoid chunked encoding - some servers might not support it.
		ContentLength: int64(len(requestBytes)),
	})
	if err != nil {
		return fmt.Errorf("goupnp: error performing SOAP HTTP request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		resp, _ := ioutil.ReadAll(response.Body)
		return fmt.Errorf("goupnp: SOAP request got HTTP %s: %s", response.Status, resp)
	}

	responseEnv := newSOAPEnvelope()
	decoder := xml.NewDecoder(response.Body)
	if err := decoder.Decode(responseEnv); err != nil {
		return fmt.Errorf("goupnp: error decoding response body: %v", err)
	}

	if responseEnv.Body.Fault != nil {
		return responseEnv.Body.Fault
	}

	if outAction != nil {
		if err := xml.Unmarshal(responseEnv.Body.RawAction, outAction); err != nil {
			return fmt.Errorf("goupnp: error unmarshalling out action: %v, %v", err, responseEnv.Body.RawAction)
		}
	}

	return nil
}

// newSOAPAction creates a soapEnvelope with the given action and arguments.
func newSOAPEnvelope() *soapEnvelope {
	return &soapEnvelope{
		EncodingStyle: soapEncodingStyle,
	}
}

// encodeRequestAction is a hacky way to create an encoded SOAP envelope
// containing the given action. Experiments with one router have shown that it
// 500s for requests where the outer default xmlns is set to the SOAP
// namespace, and then reassigning the default namespace within that to the
// service namespace. Hand-coding the outer XML to work-around this.
func encodeRequestAction(actionNamespace, actionName string, inAction interface{}) ([]byte, error) {
	requestBuf := new(bytes.Buffer)
	requestBuf.WriteString(soapPrefix)
	requestBuf.WriteString(`<u:`)
	xml.EscapeText(requestBuf, []byte(actionName))
	requestBuf.WriteString(` xmlns:u="`)
	xml.EscapeText(requestBuf, []byte(actionNamespace))
	requestBuf.WriteString(`">`)
	if inAction != nil {
		if err := encodeRequestArgs(requestBuf, inAction); err != nil {
			return nil, err
		}
	}
	requestBuf.WriteString(`</u:`)
	xml.EscapeText(requestBuf, []byte(actionName))
	requestBuf.WriteString(`>`)
	requestBuf.WriteString(soapSuffix)
	return requestBuf.Bytes(), nil
}

func encodeRequestArgs(w *bytes.Buffer, inAction interface{}) error {
	in := reflect.Indirect(reflect.ValueOf(inAction))
	if in.Kind() != reflect.Struct {
		return fmt.Errorf("goupnp: SOAP inAction is not a struct but of type %v", in.Type())
	}
	enc := xml.NewEncoder(w)
	nFields := in.NumField()
	inType := in.Type()
	for i := 0; i < nFields; i++ {
		field := inType.Field(i)
		argName := field.Name
		if nameOverride := field.Tag.Get("soap"); nameOverride != "" {
			argName = nameOverride
		}
		value := in.Field(i)
		if value.Kind() != reflect.String {
			return fmt.Errorf("goupnp: SOAP arg %q is not of type string, but of type %v", argName, value.Type())
		}
		if err := enc.EncodeElement(value.Interface(), xml.StartElement{xml.Name{"", argName}, nil}); err != nil {
			return fmt.Errorf("goupnp: error encoding SOAP arg %q: %v", argName, err)
		}
	}
	enc.Flush()
	return nil
}

type soapEnvelope struct {
	XMLName       xml.Name `xml:"http://schemas.xmlsoap.org/soap/envelope/ Envelope"`
	EncodingStyle string   `xml:"http://schemas.xmlsoap.org/soap/envelope/ encodingStyle,attr"`
	Body          soapBody `xml:"http://schemas.xmlsoap.org/soap/envelope/ Body"`
}

type soapBody struct {
	Fault     *SOAPFaultError `xml:"Fault"`
	RawAction []byte          `xml:",innerxml"`
}

// SOAPFaultError implements error, and contains SOAP fault information.
type SOAPFaultError struct {
	FaultCode   string `xml:"faultcode"`
	FaultString string `xml:"faultstring"`
	Detail      string `xml:"detail"`
}

func (err *SOAPFaultError) Error() string {
	return fmt.Sprintf("SOAP fault: %s", err.FaultString)
}
