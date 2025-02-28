package gosoap

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html/charset"
)

// HeaderParams holds params specific to the header
type HeaderParams map[string]interface{}

// Params type is used to set the params in soap request
type Params map[string]interface{}

// SoapClient return new *Client to handle the requests with the WSDL
func SoapClient(wsdl string) (*Client, error) {
	_, err := url.Parse(wsdl)
	if err != nil {
		return nil, err
	}

	c := &Client{
		wsdl:       wsdl,
		HttpClient: &http.Client{},
	}

	return c, nil
}

// Client struct hold all the informations about WSDL,
// request and response of the server
type Client struct {
	HttpClient   *http.Client
	URL          string
	HeaderName   string
	HeaderParams HeaderParams
	Definitions  *wsdlDefinitions
	// Must be set before first request otherwise has no effect, minimum is 15 minutes.
	RefreshDefinitionsAfter time.Duration
	Username                string
	Password                string

	once                 sync.Once
	definitionsErr       error
	onRequest            sync.WaitGroup
	onDefinitionsRefresh sync.WaitGroup
	wsdl                 string
}

// Call call's the method m with Params p
func (c *Client) Call(m string, p Params) (res *Response, err error) {
	return c.Do(NewRequest(m, p))
}

// Call call's by struct
func (c *Client) CallByStruct(s RequestStruct) (res *Response, err error) {
	req, err := NewRequestByStruct(s)
	if err != nil {
		return nil, err
	}

	return c.Do(req)
}

func (c *Client) waitAndRefreshDefinitions(d time.Duration) {
	for {
		time.Sleep(d)
		c.onRequest.Wait()
		c.onDefinitionsRefresh.Add(1)
		c.initWsdl()
		c.onDefinitionsRefresh.Done()
	}
}

func (c *Client) initWsdl() {
	c.Definitions, c.definitionsErr = c.getWsdlDefinitions()
	if c.definitionsErr == nil {
		c.URL = strings.TrimSuffix(c.Definitions.TargetNamespace, "/")
	}
}

func (c *Client) SetWSDL(wsdl string) {
	c.onRequest.Wait()
	c.onDefinitionsRefresh.Wait()
	c.onRequest.Add(1)
	c.onDefinitionsRefresh.Add(1)
	defer c.onRequest.Done()
	defer c.onDefinitionsRefresh.Done()
	c.wsdl = wsdl
	c.initWsdl()
}

// Process Soap Request
func (c *Client) Do(req *Request) (res *Response, err error) {
	c.onDefinitionsRefresh.Wait()
	c.onRequest.Add(1)
	defer c.onRequest.Done()

	c.once.Do(func() {
		c.initWsdl()
		// 15 minute to prevent abuse.
		if c.RefreshDefinitionsAfter >= 15*time.Minute {
			go c.waitAndRefreshDefinitions(c.RefreshDefinitionsAfter)
		}
	})

	if c.definitionsErr != nil {
		return nil, c.definitionsErr
	}

	if c.Definitions == nil {
		return nil, errors.New("wsdl definitions not found")
	}

	if c.Definitions.Services == nil {
		return nil, errors.New("No Services found in wsdl definitions")
	}

	p := &process{
		Client:     c,
		Request:    req,
		SoapAction: c.Definitions.GetSoapActionFromWsdlOperation(req.Method),
	}

	if p.SoapAction == "" {
		p.SoapAction = fmt.Sprintf("%s/%s", c.URL, req.Method)
	}

	p.Payload, err = xml.MarshalIndent(p, "", "    ")
	if err != nil {
		return nil, err
	}

	b, err := p.doRequest(c.Definitions.Services[0].Ports[0].SoapAddresses[0].Location)
	if err != nil {
		return nil, ErrorWithPayload{err, p.Payload}
	}

	var soap SoapEnvelope
	// err = xml.Unmarshal(b, &soap)
	// error: xml: encoding "ISO-8859-1" declared but Decoder.CharsetReader is nil
	// https://stackoverflow.com/questions/6002619/unmarshal-an-iso-8859-1-xml-input-in-go
	// https://github.com/golang/go/issues/8937

	decoder := xml.NewDecoder(bytes.NewReader(b))
	decoder.CharsetReader = charset.NewReaderLabel
	err = decoder.Decode(&soap)

	res = &Response{
		Body:    soap.Body.Contents,
		Header:  soap.Header.Contents,
		Payload: p.Payload,
	}
	if err != nil {
		return res, ErrorWithPayload{err, p.Payload}
	}

	return res, nil
}

type process struct {
	Client     *Client
	Request    *Request
	SoapAction string
	Payload    []byte
}

// doRequest makes new request to the server using the c.Method, c.URL and the body.
// body is enveloped in Do method
func (p *process) doRequest(url string) ([]byte, error) {
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(p.Payload))
	if err != nil {
		return nil, err
	}

	if p.Client.Username != "" && p.Client.Password != "" {
		req.SetBasicAuth(p.Client.Username, p.Client.Password)
	}

	req.ContentLength = int64(len(p.Payload))

	req.Header.Add("Content-Type", "text/xml;charset=UTF-8")
	req.Header.Add("Accept", "text/xml")
	req.Header.Add("SOAPAction", p.SoapAction)

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return ioutil.ReadAll(resp.Body)
}

func (p *process) httpClient() *http.Client {
	if p.Client.HttpClient != nil {
		return p.Client.HttpClient
	}
	return http.DefaultClient
}

type ErrorWithPayload struct {
	error
	Payload []byte
}

func GetPayloadFromError(err error) []byte {
	if err, ok := err.(ErrorWithPayload); ok {
		return err.Payload
	}
	return nil
}

// SoapEnvelope struct
type SoapEnvelope struct {
	XMLName struct{} `xml:"Envelope"`
	Header  SoapHeader
	Body    SoapBody
}

// SoapHeader struct
type SoapHeader struct {
	XMLName  struct{} `xml:"Header"`
	Contents []byte   `xml:",innerxml"`
}

// SoapBody struct
type SoapBody struct {
	XMLName  struct{} `xml:"Body"`
	Contents []byte   `xml:",innerxml"`
}
