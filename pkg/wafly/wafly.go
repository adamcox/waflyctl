/*
 * WAF provisioning tool
 *
 * Copyright (c) 2018-2019 Fastly Inc.

 * Author: Jose Enrique Hernandez
 */

package wafly

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/sethvargo/go-fastly/fastly"
	"gopkg.in/resty.v1"
)

var (

	//Info level logging
	Info *log.Logger

	//Warning level logging
	Warning *log.Logger

	//Error level logging
	Error *log.Logger
)

// TOMLConfig is the applications config file
type TOMLConfig struct {
	Logpath       string
	APIEndpoint   string
	Tags          []string
	Publisher     []string
	Action        string
	Rules         []int64
	DisabledRules []int64
	Owasp         owaspSettings
	Weblog        WeblogSettings
	Waflog        WaflogSettings
	Vclsnippet    VCLSnippetSettings
	Response      ResponseSettings
	Prefetch      PrefetchSettings
}

// Backup is a backup of the rule status for a WAF
type Backup struct {
	ServiceID string
	ID        string
	Updated   time.Time
	Disabled  []string
	Block     []string
	Log       []string
	Owasp     owaspSettings
}

type owaspSettings struct {
	AllowedHTTPVersions              string
	AllowedMethods                   string
	AllowedRequestContentType        string
	AllowedRequestContentTypeCharset string
	ArgLength                        int
	ArgNameLength                    int
	CombinedFileSizes                int
	CriticalAnomalyScore             int
	CRSValidateUTF8Encoding          bool
	ErrorAnomalyScore                int
	HTTPViolationScoreThreshold      int
	InboundAnomalyScoreThreshold     int
	LFIScoreThreshold                int
	MaxFileSize                      int
	MaxNumArgs                       int
	NoticeAnomalyScore               int
	ParanoiaLevel                    int
	PHPInjectionScoreThreshold       int
	RCEScoreThreshold                int
	RestrictedExtensions             string
	RestrictedHeaders                string
	RFIScoreThreshold                int
	SessionFixationScoreThreshold    int
	SQLInjectionScoreThreshold       int
	XSSScoreThreshold                int
	TotalArgLength                   int
	WarningAnomalyScore              int
}

// WeblogSettings parameters for logs in config file
type WeblogSettings struct {
	Name        string
	Address     string
	Port        uint
	Tlscacert   string
	Tlshostname string
	Format      string
	Expiry      uint
}

// VCLSnippetSettings parameters for snippets in config file
type VCLSnippetSettings struct {
	Name     string
	Content  string
	Type     fastly.SnippetType
	Priority int
	Dynamic  int
}

// WaflogSettings parameters from config
type WaflogSettings struct {
	Name        string
	Address     string
	Port        uint
	Tlscacert   string
	Tlshostname string
	Format      string
}

// ResponseSettings parameters from config
type ResponseSettings struct {
	Name           string
	HTTPStatusCode uint
	HTTPResponse   string
	ContentType    string
	Content        string
}

// PrefetchSettings parameters from config
type PrefetchSettings struct {
	Name      string
	Statement string
	Type      string
	Priority  int
}

// RuleList contains list of rules
type RuleList struct {
	Data  []Rule
	Links struct {
		Last  string `json:"last"`
		First string `json:"first"`
		Next  string `json:"next"`
	} `json:"links"`

	Meta struct {
		CurrentPage int `json:"current_page"`
		PerPage     int `json:"per_page"`
		RecordCount int `json:"record_count"`
		TotalPages  int `json:"total_pages"`
	} `json:"meta"`
}

// Rule from Fastly API
type Rule struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Attributes struct {
		Message       string      `json:"message"`
		Status        string      `json:"status"`
		Publisher     string      `json:"publisher"`
		ParanoiaLevel int         `json:"paranoia_level"`
		Revision      int         `json:"revision"`
		Severity      interface{} `json:"severity"`
		Version       interface{} `json:"version"`
		RuleID        string      `json:"rule_id"`
		ModsecRuleID  string      `json:"modsec_rule_id"`
		UniqueRuleID  string      `json:"unique_rule_id"`
		Source        interface{} `json:"source"`
		Vcl           interface{} `json:"vcl"`
	} `json:"attributes"`
}

// PagesOfRules contains a list of rulelist
type PagesOfRules struct {
	page []RuleList
}

// PagesOfConfigurationSets contains a list of ConfigSetList
type PagesOfConfigurationSets struct {
	page []ConfigSetList
}

// ConfigSetList contains a list of configuration set and its metadata
type ConfigSetList struct {
	Data  []ConfigSet
	Links struct {
		Last  string `json:"last"`
		First string `json:"first"`
		Next  string `json:"next"`
	} `json:"links"`
	Meta struct {
		CurrentPage int `json:"current_page"`
		PerPage     int `json:"per_page"`
		RecordCount int `json:"record_count"`
		TotalPages  int `json:"total_pages"`
	} `json:"meta"`
}

// ConfigSet defines details of a configuration set
type ConfigSet struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Attributes struct {
		Active bool   `json:"active"`
		Name   string `json:"name"`
	} `json:"attributes"`
}

func GetActiveVersion(client fastly.Client, serviceID string) int {
	service, err := client.GetService(&fastly.GetServiceInput{
		ID: serviceID,
	})
	if err != nil {
		Error.Fatalf("Cannot get service %q: GetService: %v\n", serviceID, err)
	}
	for _, version := range service.Versions {
		if version.Active {
			return version.Number
		}
	}
	Error.Fatal("No active version found (wrong service id?). Aborting")
	return 0
}

func CloneVersion(client fastly.Client, serviceID string, activeVersion int) int {
	version, err := client.CloneVersion(&fastly.CloneVersionInput{
		Service: serviceID,
		Version: activeVersion,
	})
	if err != nil {
		Error.Fatalf("Cannot clone version %d: CloneVersion: %v\n", activeVersion, err)
	}
	Info.Printf("New version %d created\n", version.Number)
	return version.Number
}

func prefetchCondition(client fastly.Client, serviceID string, config TOMLConfig, version int) {
	conditions, err := client.ListConditions(&fastly.ListConditionsInput{
		Service: serviceID,
		Version: version,
	})
	if err != nil {
		Error.Fatalf("Cannot create prefetch condition %q: ListConditions: %v\n", config.Prefetch.Name, err)
	}

	if !conditionExists(conditions, config.Prefetch.Name) {
		_, err = client.CreateCondition(&fastly.CreateConditionInput{
			Service:   serviceID,
			Version:   version,
			Name:      config.Prefetch.Name,
			Statement: config.Prefetch.Statement,
			Type:      config.Prefetch.Type,
			Priority:  10,
		})
		if err != nil {
			Error.Fatalf("Cannot create prefetch condition %q: CreateCondition: %v\n", config.Prefetch.Name, err)
		}
		Info.Printf("Prefetch condition %q created\n", config.Prefetch.Name)
	} else {
		Warning.Printf("Prefetch condition %q already exists, skipping\n", config.Prefetch.Name)
	}

}

func responseObject(client fastly.Client, serviceID string, config TOMLConfig, version int) {
	responses, err := client.ListResponseObjects(&fastly.ListResponseObjectsInput{
		Service: serviceID,
		Version: version,
	})
	if err != nil {
		Error.Fatalf("Cannot create response object %q: ListResponseObjects: %v\n", config.Response.Name, err)
	}
	for _, response := range responses {
		if strings.EqualFold(response.Name, config.Response.Name) {
			Warning.Printf("Response object %q already exists, skipping\n", config.Response.Name)
			return
		}
	}
	_, err = client.CreateResponseObject(&fastly.CreateResponseObjectInput{
		Service:     serviceID,
		Version:     version,
		Name:        config.Response.Name,
		Status:      config.Response.HTTPStatusCode,
		Response:    config.Response.HTTPResponse,
		Content:     config.Response.Content,
		ContentType: config.Response.ContentType,
	})
	if err != nil {
		Error.Fatalf("Cannot create response object %q: CreateResponseObject: %v\n", config.Response.Name, err)
	}
	Info.Printf("Response object %q created\n", config.Response.Name)
}

func VclSnippet(client fastly.Client, serviceID string, config TOMLConfig, version int) {
	snippets, err := client.ListSnippets(&fastly.ListSnippetsInput{
		Service: serviceID,
		Version: version,
	})
	if err != nil {
		Error.Fatalf("Cannot create VCL snippet %q: ListSnippets: %v\n", config.Vclsnippet.Name, err)
	}
	for _, snippet := range snippets {
		if snippet.Name == config.Vclsnippet.Name {
			Warning.Printf("VCL snippet %q already exists, skipping\n", config.Vclsnippet.Name)
			return
		}
	}
	_, err = client.CreateSnippet(&fastly.CreateSnippetInput{
		Service:  serviceID,
		Version:  version,
		Name:     config.Vclsnippet.Name,
		Priority: config.Vclsnippet.Priority,
		Dynamic:  config.Vclsnippet.Dynamic,
		Content:  config.Vclsnippet.Content,
		Type:     config.Vclsnippet.Type,
	})
	if err != nil {
		Error.Fatalf("Cannot create VCL snippet %q: CreateSnippet: %v\n", config.Vclsnippet.Name, err)
	}
	Info.Printf("VCL snippet %q created\n", config.Vclsnippet.Name)
}

func FastlyLogging(client fastly.Client, serviceID string, config TOMLConfig, version int) {
	_, err := client.CreateSyslog(&fastly.CreateSyslogInput{
		Service:       serviceID,
		Version:       version,
		Name:          config.Weblog.Name,
		Address:       config.Weblog.Address,
		Port:          config.Weblog.Port,
		UseTLS:        fastly.CBool(true),
		IPV4:          config.Weblog.Address,
		TLSCACert:     config.Weblog.Tlscacert,
		TLSHostname:   config.Weblog.Tlshostname,
		Format:        config.Weblog.Format,
		FormatVersion: 2,
		MessageType:   "blank",
	})
	switch {
	case err == nil:
		Info.Printf("Logging endpoint %q created\n", config.Weblog.Name)
	case strings.Contains(err.Error(), "Duplicate record"):
		Warning.Printf("Logging endpoint %q already exists, skipping\n", config.Weblog.Name)
	default:
		Error.Fatalf("Cannot create logging endpoint %q: CreateSyslog: %v\n", config.Weblog.Name, err)
	}
	_, err = client.CreateSyslog(&fastly.CreateSyslogInput{
		Service:       serviceID,
		Version:       version,
		Name:          config.Waflog.Name,
		Address:       config.Waflog.Address,
		Port:          config.Waflog.Port,
		UseTLS:        fastly.CBool(true),
		IPV4:          config.Waflog.Address,
		TLSCACert:     config.Waflog.Tlscacert,
		TLSHostname:   config.Waflog.Tlshostname,
		Format:        config.Waflog.Format,
		FormatVersion: 2,
		MessageType:   "blank",
		Placement:     "waf_debug",
	})
	switch {
	case err == nil:
		Info.Printf("Logging endpoint %q created\n", config.Waflog.Name)
	case strings.Contains(err.Error(), "Duplicate record"):
		Warning.Printf("Logging endpoint %q already exists, skipping\n", config.Waflog.Name)
	default:
		Error.Fatalf("Cannot create logging endpoint %q: CreateSyslog: %v\n", config.Waflog.Name, err)
	}
}

func wafContainer(client fastly.Client, serviceID string, config TOMLConfig, version int) string {
	waf, err := client.CreateWAF(&fastly.CreateWAFInput{
		Service:           serviceID,
		Version:           version,
		PrefetchCondition: config.Prefetch.Name,
		Response:          config.Response.Name,
	})
	if err != nil {
		Error.Fatalf("Cannot create WAF: CreateWAF: %v\n", err)
	}
	Info.Printf("WAF %q created\n", waf.ID)
	return waf.ID
}

func CreateOWASP(client fastly.Client, serviceID string, config TOMLConfig, wafID string) {
	var created bool
	var err error
	owasp, _ := client.GetOWASP(&fastly.GetOWASPInput{
		Service: serviceID,
		ID:      wafID,
	})
	if owasp.ID == "" {
		owasp, err = client.CreateOWASP(&fastly.CreateOWASPInput{
			Service: serviceID,
			ID:      wafID,
		})
		if err != nil {
			Error.Fatalf("%v\n", err)
		}
		created = true
	}
	owasp, err = client.UpdateOWASP(&fastly.UpdateOWASPInput{
		Service:                          serviceID,
		ID:                               wafID,
		OWASPID:                          owasp.ID,
		AllowedHTTPVersions:              config.Owasp.AllowedHTTPVersions,
		AllowedMethods:                   config.Owasp.AllowedMethods,
		AllowedRequestContentType:        config.Owasp.AllowedRequestContentType,
		AllowedRequestContentTypeCharset: config.Owasp.AllowedRequestContentTypeCharset,
		ArgLength:                        config.Owasp.ArgLength,
		ArgNameLength:                    config.Owasp.ArgNameLength,
		CombinedFileSizes:                config.Owasp.CombinedFileSizes,
		CriticalAnomalyScore:             config.Owasp.CriticalAnomalyScore,
		CRSValidateUTF8Encoding:          config.Owasp.CRSValidateUTF8Encoding,
		ErrorAnomalyScore:                config.Owasp.ErrorAnomalyScore,
		HTTPViolationScoreThreshold:      config.Owasp.HTTPViolationScoreThreshold,
		InboundAnomalyScoreThreshold:     config.Owasp.InboundAnomalyScoreThreshold,
		LFIScoreThreshold:                config.Owasp.LFIScoreThreshold,
		MaxFileSize:                      config.Owasp.MaxFileSize,
		MaxNumArgs:                       config.Owasp.MaxNumArgs,
		NoticeAnomalyScore:               config.Owasp.NoticeAnomalyScore,
		ParanoiaLevel:                    config.Owasp.ParanoiaLevel,
		PHPInjectionScoreThreshold:       config.Owasp.PHPInjectionScoreThreshold,
		RCEScoreThreshold:                config.Owasp.RCEScoreThreshold,
		RestrictedExtensions:             config.Owasp.RestrictedExtensions,
		RestrictedHeaders:                config.Owasp.RestrictedHeaders,
		RFIScoreThreshold:                config.Owasp.RFIScoreThreshold,
		SessionFixationScoreThreshold:    config.Owasp.SessionFixationScoreThreshold,
		SQLInjectionScoreThreshold:       config.Owasp.SQLInjectionScoreThreshold,
		XSSScoreThreshold:                config.Owasp.XSSScoreThreshold,
		TotalArgLength:                   config.Owasp.TotalArgLength,
		WarningAnomalyScore:              config.Owasp.WarningAnomalyScore,
	})
	if err != nil {
		Error.Fatalf("%v\n", err)
	}
	if created {
		Info.Println("OWASP settings created with the following settings:")
	} else {
		Info.Println("OWASP settings updated with the following settings:")
	}
	Info.Println(" - AllowedHTTPVersions:", owasp.AllowedHTTPVersions)
	Info.Println(" - AllowedMethods:", owasp.AllowedMethods)
	Info.Println(" - AllowedRequestContentType:", owasp.AllowedRequestContentType)
	Info.Println(" - AllowedRequestContentTypeCharset:", owasp.AllowedRequestContentTypeCharset)
	Info.Println(" - ArgLength:", owasp.ArgLength)
	Info.Println(" - ArgNameLength:", owasp.ArgNameLength)
	Info.Println(" - CombinedFileSizes:", owasp.CombinedFileSizes)
	Info.Println(" - CriticalAnomalyScore:", owasp.CriticalAnomalyScore)
	Info.Println(" - CRSValidateUTF8Encoding:", owasp.CRSValidateUTF8Encoding)
	Info.Println(" - ErrorAnomalyScore:", owasp.ErrorAnomalyScore)
	Info.Println(" - HTTPViolationScoreThreshold:", owasp.HTTPViolationScoreThreshold)
	Info.Println(" - InboundAnomalyScoreThreshold:", owasp.InboundAnomalyScoreThreshold)
	Info.Println(" - LFIScoreThreshold:", owasp.LFIScoreThreshold)
	Info.Println(" - MaxFileSize:", owasp.MaxFileSize)
	Info.Println(" - MaxNumArgs:", owasp.MaxNumArgs)
	Info.Println(" - NoticeAnomalyScore:", owasp.NoticeAnomalyScore)
	Info.Println(" - ParanoiaLevel:", owasp.ParanoiaLevel)
	Info.Println(" - PHPInjectionScoreThreshold:", owasp.PHPInjectionScoreThreshold)
	Info.Println(" - RCEScoreThreshold:", owasp.RCEScoreThreshold)
	Info.Println(" - RestrictedHeaders:", owasp.RestrictedHeaders)
	Info.Println(" - RFIScoreThreshold:", owasp.RFIScoreThreshold)
	Info.Println(" - SessionFixationScoreThreshold:", owasp.SessionFixationScoreThreshold)
	Info.Println(" - SQLInjectionScoreThreshold:", owasp.SQLInjectionScoreThreshold)
	Info.Println(" - XssScoreThreshold:", owasp.XSSScoreThreshold)
	Info.Println(" - TotalArgLength:", owasp.TotalArgLength)
	Info.Println(" - WarningAnomalyScore:", owasp.WarningAnomalyScore)
}

// DeleteLogsCall removes logging endpoints and any logging conditions.
func DeleteLogsCall(client fastly.Client, serviceID string, config TOMLConfig, version int) bool {

	//Get a list of SysLogs
	slogs, err := client.ListSyslogs(&fastly.ListSyslogsInput{
		Service: serviceID,
		Version: version,
	})
	if err != nil {
		Error.Println(err)
		return false
	}

	//drop syslogs if they exist
	if sysLogExists(slogs, config.Weblog.Name) {
		Info.Printf("Deleting Web logging endpoint: %q\n", config.Weblog.Name)
		err = client.DeleteSyslog(&fastly.DeleteSyslogInput{
			Service: serviceID,
			Version: version,
			Name:    config.Weblog.Name,
		})
		if err != nil {
			fmt.Println(err)
			return false
		}
	}

	if sysLogExists(slogs, config.Waflog.Name) {
		Info.Printf("Deleting WAF logging endpoint: %q\n", config.Waflog.Name)
		err = client.DeleteSyslog(&fastly.DeleteSyslogInput{
			Service: serviceID,
			Version: version,
			Name:    config.Waflog.Name,
		})
		if err != nil {
			fmt.Println(err)
			return false
		}
	}

	//first find if we have any PX conditions
	conditions, err := client.ListConditions(&fastly.ListConditionsInput{
		Service: serviceID,
		Version: version,
	})
	if err != nil {
		Error.Println(err)
		return false
	}

	//remove logging conditions (and expiry conditions)
	if conditionExists(conditions, "waf-soc-logging") {
		Info.Println("Deleting logging condition: 'waf-soc-logging'")
		err = client.DeleteCondition(&fastly.DeleteConditionInput{
			Service: serviceID,
			Version: version,
			Name:    "waf-soc-logging",
		})
		if err != nil {
			Error.Println(err)
			return false
		}
	}
	if conditionExists(conditions, "waf-soc-logging-with-expiry") {
		Info.Println("Deleting logging condition: 'waf-soc-logging-with-expiry'")
		err = client.DeleteCondition(&fastly.DeleteConditionInput{
			Service: serviceID,
			Version: version,
			Name:    "waf-soc-logging-with-expiry",
		})
		if err != nil {
			Error.Println(err)
			return false
		}
	}

	//Legacy conditions
	//remove PerimeterX logging condition (if exists)
	if conditionExists(conditions, "waf-soc-with-px") {
		Info.Println("Deleting Legacy PerimeterX logging condition: 'waf-soc-with-px'")
		err = client.DeleteCondition(&fastly.DeleteConditionInput{
			Service: serviceID,
			Version: version,
			Name:    "waf-soc-with-px",
		})
		if err != nil {
			Error.Println(err)
			return false
		}

	}

	//remove shielding logging condition (if exists)
	if conditionExists(conditions, "waf-soc-with-shielding") {
		Info.Println("Deleting Legacy Shielding logging condition: 'waf-soc-with-shielding'")
		err = client.DeleteCondition(&fastly.DeleteConditionInput{
			Service: serviceID,
			Version: version,
			Name:    "waf-soc-with-shielding",
		})
		if err != nil {
			Error.Println(err)
			return false
		}
	}

	return true

}

// conditionExists iterates through the given slice of conditions and returns
// whether the given name exists in the collection
func conditionExists(conds []*fastly.Condition, name string) bool {
	for _, c := range conds {
		if strings.EqualFold(c.Name, name) {
			return true
		}
	}
	return false
}

// sysLogExists iterates through the given slice of syslogs and returns
// whether the given name exists in the collection
func sysLogExists(slogs []*fastly.Syslog, name string) bool {
	for _, sl := range slogs {
		if strings.EqualFold(sl.Name, name) {
			return true
		}
	}
	return false
}

// DeprovisionWAF removes a WAF from a service
func DeprovisionWAF(client fastly.Client, serviceID, apiKey string, config TOMLConfig, version int) bool {
	/*
		To Remove
		1. Delete response
		2. Delete prefetch
		3. Delete WAF
	*/

	//get current waf objects
	wafs, err := client.ListWAFs(&fastly.ListWAFsInput{
		Service: serviceID,
		Version: version,
	})

	if err != nil {
		Error.Fatal(err)
		return false
	}

	if len(wafs) == 0 {
		Error.Printf("No WAF object exists in current service %s version #%v .. exiting\n", serviceID, version)
		return false
	}

	//get list of conditions
	//first find if we have any PX conditions
	conditions, err := client.ListConditions(&fastly.ListConditionsInput{
		Service: serviceID,
		Version: version,
	})
	if err != nil {
		Error.Fatal(err)
		return false
	}

	for index, waf := range wafs {

		//remove WAF Logging
		result := DeleteLogsCall(client, serviceID, config, version)
		Info.Printf("Deleting WAF #%v Logging\n", index+1)
		if !result {
			Error.Printf("Deleting WAF #%v Logging.\n", index+1)
		}

		Info.Printf("Deleting WAF #%v Container\n", index+1)
		//remove WAF container
		err = client.DeleteWAF(&fastly.DeleteWAFInput{
			Service: serviceID,
			Version: version,
			ID:      waf.ID,
		})
		if err != nil {
			Error.Print(err)
			return false
		}

		//remove WAF Response Object
		Info.Printf("Deleting WAF #%v Response Object\n", index+1)
		err = client.DeleteResponseObject(&fastly.DeleteResponseObjectInput{
			Service: serviceID,
			Version: version,
			Name:    "WAF_Response",
		})
		if err != nil {
			Error.Print(err)
			return false
		}

		//remove WAF Prefetch condition (if exists)
		if conditionExists(conditions, "WAF_Prefetch") {
			Info.Printf("Deleting WAF #%v Prefetch Condition\n", index+1)
			err = client.DeleteCondition(&fastly.DeleteConditionInput{
				Service: serviceID,
				Version: version,
				Name:    "WAF_Prefetch",
			})
			if err != nil {
				Error.Print(err)
				return false
			}
		}

		//remove VCL Snippet
		Info.Printf("Deleting WAF #%v VCL Snippet\n", index+1)
		apiCall := config.APIEndpoint + "/service/" + serviceID + "/version/" + strconv.Itoa(version) + "/snippet/" + config.Vclsnippet.Name
		//get list of current snippets
		_, err := resty.R().
			SetHeader("Accept", "application/json").
			SetHeader("Fastly-Key", apiKey).
			Delete(apiCall)

		//check if we had an issue with our call
		if err != nil {
			Error.Printf("Deleting WAF #%v VCL Snippet\n", index+1)
		}

	}

	return true
}

func ProvisionWAF(client fastly.Client, serviceID string, config TOMLConfig, version int) string {
	prefetchCondition(client, serviceID, config, version)

	responseObject(client, serviceID, config, version)

	VclSnippet(client, serviceID, config, version)

	wafID := wafContainer(client, serviceID, config, version)

	CreateOWASP(client, serviceID, config, wafID)

	// In order to create the logging endpoints WAF must be
	// created first. ¯\_(ツ)_/¯
	FastlyLogging(client, serviceID, config, version)

	return wafID
}

func ValidateVersion(client fastly.Client, serviceID string, version int) bool {
	valid, _, err := client.ValidateVersion(&fastly.ValidateVersionInput{
		Service: serviceID,
		Version: version,
	})
	if err != nil {
		Error.Fatal(err)
		return false
	}
	if !valid {
		Error.Println("Version invalid")
		return false
	}
	Info.Printf("Config Version %v validated. Remember to activate it\n", version)
	return true

}

func PublisherConfig(apiEndpoint, apiKey, serviceID, wafID string, config TOMLConfig) bool {

	for _, publisher := range config.Publisher {

		//set our API call
		apiCall := apiEndpoint + "/wafs/rules?filter[publisher]=" + publisher + "&page[number]=1"

		resp, err := resty.R().
			SetHeader("Accept", "application/vnd.api+json").
			SetHeader("Fastly-Key", apiKey).
			SetHeader("Content-Type", "application/vnd.api+json").
			Get(apiCall)

		//check if we had an issue with our call
		if err != nil {
			Error.Println("Error with API call: " + apiCall)
			Error.Println(resp.String())
			return false
		}

		//unmarshal the response and extract the rules
		body := RuleList{}

		json.Unmarshal([]byte(resp.String()), &body)

		if len(body.Data) == 0 {
			Error.Println("No Fastly Rules found")
			return false
		}

		result := PagesOfRules{[]RuleList{}}
		result.page = append(result.page, body)

		currentpage := body.Meta.CurrentPage
		totalpages := body.Meta.TotalPages

		Info.Printf("Read Total Pages: %d with %d rules\n", body.Meta.TotalPages, body.Meta.RecordCount)

		// iterate through pages collecting all rules
		for currentpage := currentpage + 1; currentpage <= totalpages; currentpage++ {

			Info.Printf("Reading page: %d out of %d\n", currentpage, totalpages)
			//set our API call
			apiCall := apiEndpoint + "/wafs/rules?filter[publisher]=" + publisher + "&page[number]=" + strconv.Itoa(currentpage)

			resp, err := resty.R().
				SetHeader("Accept", "application/vnd.api+json").
				SetHeader("Fastly-Key", apiKey).
				SetHeader("Content-Type", "application/vnd.api+json").
				Get(apiCall)

			//check if we had an issue with our call
			if err != nil {
				Error.Println("Error with API call: " + apiCall)
				Error.Println(resp.String())
				return false
			}

			//unmarshal the response and extract the service id
			body := RuleList{}
			json.Unmarshal([]byte(resp.String()), &body)
			result.page = append(result.page, body)
		}
		Info.Println("- Publisher ", publisher)
		for _, p := range result.page {
			for _, r := range p.Data {

				//set rule action on our tags
				apiCall := apiEndpoint + "/service/" + serviceID + "/wafs/" + wafID + "/rules/" + r.ID + "/rule_status"

				resp, err := resty.R().
					SetHeader("Accept", "application/vnd.api+json").
					SetHeader("Fastly-Key", apiKey).
					SetHeader("Content-Type", "application/vnd.api+json").
					SetBody(`{"data": {"attributes": {"status": "` + config.Action + `"},"id": "` + wafID + `-` + r.ID + `","type": "rule_status"}}`).
					Patch(apiCall)

				//check if we had an issue with our call
				if err != nil {
					Error.Println("Error with API call: " + apiCall)
					Error.Println(resp.String())
					os.Exit(1)
				}

				//check if our response was ok
				if resp.Status() == "200 OK" {
					Info.Printf("Rule %s was configured in the WAF with action %s\n", r.ID, config.Action)
				} else {
					Error.Printf("Could not set status: %s on rule tag: %s the response was: %s\n", config.Action, r.ID, resp.String())
				}
			}
		}

	}

	return true

}

func TagsConfig(apiEndpoint, apiKey, serviceID, wafID string, config TOMLConfig, forceStatus bool) {
	//Work on Tags first
	//API Endpoint to call for domain searches
	apiCall := apiEndpoint + "/wafs/tags"

	//make the call

	for _, tag := range config.Tags {

		resp, err := resty.R().
			SetQueryString(fmt.Sprintf("filter[name]=%s&include=rules", tag)).
			SetHeader("Accept", "application/vnd.api+json").
			SetHeader("Fastly-Key", apiKey).
			Get(apiCall)

		//check if we had an issue with our call
		if err != nil {
			Error.Println("Error with API call: " + apiCall)
			os.Exit(1)
		}

		//unmarshal the response and extract the service id
		body := RuleList{}
		json.Unmarshal([]byte(resp.String()), &body)

		if len(body.Data) == 0 {
			Error.Printf("Could not find any rules with tag: %s please make sure it exists..moving to the next tag\n", tag)
			continue
		}

		//set rule action on our tags
		apiCall := apiEndpoint + "/service/" + serviceID + "/wafs/" + wafID + "/rule_statuses"

		resp, err = resty.R().
			SetHeader("Accept", "application/vnd.api+json").
			SetHeader("Fastly-Key", apiKey).
			SetHeader("Content-Type", "application/vnd.api+json").
			SetBody(fmt.Sprintf(`{"data": {"attributes": {"status": "%s", "name": "%s", "force": %t}, "id": "%s", "type": "rule_status"}}`, config.Action, tag, forceStatus, wafID)).
			Post(apiCall)

		//check if we had an issue with our call
		if err != nil {
			Error.Println("Error with API call: " + apiCall)
			Error.Println(resp.String())
			os.Exit(1)
		}

		//check if our response was ok
		if resp.Status() == "200 OK" {
			Info.Printf("%s %d rule on the WAF for tag: %s\n", config.Action, len(body.Data), tag)
		} else {
			Error.Printf("Could not set status: %s on rule tag: %s the response was: %s\n", config.Action, tag, resp.String())
		}
	}
}

func ChangeStatus(apiEndpoint, apiKey, wafID, status string) {
	apiCall := apiEndpoint + "/wafs/" + wafID + "/" + status

	resp, err := resty.R().
		SetHeader("Accept", "application/vnd.api+json").
		SetHeader("Fastly-Key", apiKey).
		SetHeader("Content-Type", "application/vnd.api+json").
		SetBody(`{"data": {"id": "` + wafID + `","type": "waf"}}`).
		Patch(apiCall)

	//check if we had an issue with our call
	if err != nil {
		Error.Println("Error with API call: " + apiCall)
		Error.Println(resp.String())
		os.Exit(1)
	}

	//check if our response was ok
	if resp.Status() == "202 Accepted" {
		Info.Printf("WAF %s status was changed to %s\n", wafID, status)
	} else {
		Error.Println("Could not change the status of WAF " + wafID + " to " + status)
		Error.Println("We received the following status code: " + resp.Status() + " with response from the API: " + resp.String())
	}

}

func RulesConfig(apiEndpoint, apiKey, serviceID, wafID string, config TOMLConfig) {
	//implement individual rule management here
	for _, rule := range config.Rules {

		ruleID := strconv.FormatInt(rule, 10)

		//set rule action on our tags
		apiCall := apiEndpoint + "/service/" + serviceID + "/wafs/" + wafID + "/rules/" + ruleID + "/rule_status"

		resp, err := resty.R().
			SetHeader("Accept", "application/vnd.api+json").
			SetHeader("Fastly-Key", apiKey).
			SetHeader("Content-Type", "application/vnd.api+json").
			SetBody(`{"data": {"attributes": {"status": "` + config.Action + `"},"id": "` + wafID + `-` + ruleID + `","type": "rule_status"}}`).
			Patch(apiCall)

		//check if we had an issue with our call
		if err != nil {
			Error.Println("Error with API call: " + apiCall)
			Error.Println(resp.String())
			os.Exit(1)
		}

		//check if our response was ok
		if resp.Status() == "200 OK" {
			Info.Printf("Rule %s was configured in the WAF with action %s\n", ruleID, config.Action)
		} else {
			Error.Printf("Could not set status: %s on rule tag: %s the response was: %s\n", config.Action, ruleID, resp.String())
		}
	}
}

// DefaultRuleDisabled disables rule IDs defined in the configuration file
func DefaultRuleDisabled(apiEndpoint, apiKey, serviceID, wafID string, config TOMLConfig) {

	//implement individual rule management here
	for _, rule := range config.DisabledRules {

		ruleID := strconv.FormatInt(rule, 10)

		//set rule action on our tags
		apiCall := apiEndpoint + "/service/" + serviceID + "/wafs/" + wafID + "/rules/" + ruleID + "/rule_status"

		resp, err := resty.R().
			SetHeader("Accept", "application/vnd.api+json").
			SetHeader("Fastly-Key", apiKey).
			SetHeader("Content-Type", "application/vnd.api+json").
			SetBody(`{"data": {"attributes": {"status": "disabled"},"id": "` + wafID + `-` + ruleID + `","type": "rule_status"}}`).
			Patch(apiCall)

		//check if we had an issue with our call
		if err != nil {
			Error.Println("Error with API call: " + apiCall)
			Error.Println(resp.String())
			os.Exit(1)
		}

		//check if our response was ok
		if resp.Status() == "200 OK" {
			Info.Printf("Rule %s was configured in the WAF with action disabled via disabledrules parameter\n", ruleID)
		} else {
			Error.Printf("Could not set status: %s on rule tag: %s the response was: %s\n", config.Action, ruleID, resp.String())
		}
	}
}

// AddLoggingCondition creates/updates logging conditions based on whether the
// user has specified withShielding, withPerimeterX and a web-log expiry.
// NOTE: PerimeterX conditions will be deprecated next major release.
func AddLoggingCondition(client fastly.Client, serviceID string, version int, config TOMLConfig, withShielding bool, withPX bool) bool {
	conditions, err := client.ListConditions(&fastly.ListConditionsInput{
		Service: serviceID,
		Version: version,
	})
	if err != nil {
		Error.Fatal(err)
		return false
	}

	// Create condition statement for Shielding & PX
	var cstmts []string
	var msgs []string
	if withShielding {
		msgs = append(msgs, "Shielding")
		cstmts = append(cstmts, "(waf.executed || fastly_info.state !~ \"(MISS|PASS)\")")
	}
	if withPX {
		msgs = append(msgs, "PerimeterX")
		cstmts = append(cstmts, "(req.http.x-request-id)")
	}

	// Create WAF Log condition (drop the old one if it exists)
	cn := "waf-soc-logging"
	if conditionExists(conditions, cn) {
		Info.Printf("Updating WAF logging condition : %q\n", cn)
		_, err = client.UpdateCondition(&fastly.UpdateConditionInput{
			Service:   serviceID,
			Version:   version,
			Name:      cn,
			Statement: strings.Join(cstmts, " && "),
			Type:      "RESPONSE",
			Priority:  10,
		})
		if err != nil {
			Error.Fatal(err)
			return false
		}
	} else {
		Info.Printf("Creating WAF logging condition : %q\n", cn)
		_, err = client.CreateCondition(&fastly.CreateConditionInput{
			Service:   serviceID,
			Version:   version,
			Name:      cn,
			Statement: strings.Join(cstmts, " && "),
			Type:      "RESPONSE",
			Priority:  10,
		})
		if err != nil {
			Error.Fatal(err)
			return false
		}
	}

	// Assign the conditions to the WAF log object
	Info.Printf("Assigning condition %q (%s) to WAF log %q\n", cn, strings.Join(msgs, ", "), config.Waflog.Name)
	_, err = client.UpdateSyslog(&fastly.UpdateSyslogInput{
		Service:           serviceID,
		Version:           version,
		Name:              config.Waflog.Name,
		ResponseCondition: cn,
	})
	if err != nil {
		Error.Fatal(err)
		return false
	}

	// If a WAF Web-Log expiry has been defined, add expiry to the condition.
	if config.Weblog.Expiry > 0 {
		cn = "waf-soc-logging-with-expiry"
		exp := time.Now().AddDate(0, 0, int(config.Weblog.Expiry)).Unix()
		cstmts = append(cstmts, fmt.Sprintf("(std.atoi(now.sec) > %d)", exp))
		msgs = append(msgs, fmt.Sprintf("%d day expiry", config.Weblog.Expiry))

		if conditionExists(conditions, cn) {
			Info.Printf("Updating WAF logging condition with %d day expiry : %q\n", config.Weblog.Expiry, cn)
			_, err = client.UpdateCondition(&fastly.UpdateConditionInput{
				Service:   serviceID,
				Version:   version,
				Name:      cn,
				Statement: strings.Join(cstmts, " && "),
				Type:      "RESPONSE",
				Priority:  10,
			})
			if err != nil {
				Error.Fatal(err)
				return false
			}
		} else {
			Info.Printf("Creating WAF logging condition with %d day expiry : %q\n", config.Weblog.Expiry, cn)
			_, err = client.CreateCondition(&fastly.CreateConditionInput{
				Service:   serviceID,
				Version:   version,
				Name:      cn,
				Statement: strings.Join(cstmts, " && "),
				Type:      "RESPONSE",
				Priority:  10,
			})
			if err != nil {
				Error.Fatal(err)
				return false
			}
		}
	} else {
		// Check for old Expires condition and clean
		if conditionExists(conditions, "waf-soc-logging-with-expiry") {
			Info.Println("Deleting logging condition: 'waf-soc-logging-with-expiry'")
			err = client.DeleteCondition(&fastly.DeleteConditionInput{
				Service: serviceID,
				Version: version,
				Name:    "waf-soc-logging-with-expiry",
			})
			if err != nil {
				Error.Fatal(err)
				return false
			}
		}
	}

	// Assign the conditions to the WAF web-log object
	Info.Printf("Assigning condition %q (%s) to web log %q\n", cn, strings.Join(msgs, ", "), config.Weblog.Name)
	_, err = client.UpdateSyslog(&fastly.UpdateSyslogInput{
		Service:           serviceID,
		Version:           version,
		Name:              config.Weblog.Name,
		ResponseCondition: cn,
	})
	if err != nil {
		Error.Fatal(err)
		return false
	}

	return true

}

// PatchRules function patches a rule set after a status of a rule has been changed
func PatchRules(serviceID, wafID string, client fastly.Client) bool {

	_, err := client.UpdateWAFRuleSets(&fastly.UpdateWAFRuleRuleSetsInput{
		Service: serviceID,
		ID:      wafID,
	})

	if err != nil {
		Error.Print(err)
		return false

	}
	return true
}

// changeConfigurationSet function allows you to change a config set for a WAF object
func SetConfigurationSet(wafID, configurationSet string, client fastly.Client) bool {

	wafs := []fastly.ConfigSetWAFs{{ID: wafID}}

	_, err := client.UpdateWAFConfigSet(&fastly.UpdateWAFConfigSetInput{
		WAFList:     wafs,
		ConfigSetID: configurationSet,
	})

	//check if we had an issue with our call
	if err != nil {
		Error.Println("Error setting configuration set ID: " + configurationSet)
		return false
	}

	return true

}

// getConfigurationSets function provides a listing of all config sets
func GetConfigurationSets(apiEndpoint, apiKey string) bool {
	//set our API call
	apiCall := apiEndpoint + "/wafs/configuration_sets"

	resp, err := resty.R().
		SetHeader("Accept", "application/vnd.api+json").
		SetHeader("Fastly-Key", apiKey).
		SetHeader("Content-Type", "application/vnd.api+json").
		Get(apiCall)

	//check if we had an issue with our call
	if err != nil {
		Error.Println("Error with API call: " + apiCall)
		Error.Println(resp.String())
		return false
	}

	//unmarshal the response and extract the service id
	body := ConfigSetList{}
	json.Unmarshal([]byte(resp.String()), &body)

	if len(body.Data) == 0 {
		Error.Println("No Configuration Sets found")
		return false
	}

	json.Unmarshal([]byte(resp.String()), &body)

	if len(body.Data) == 0 {
		Error.Println("No Fastly Rules found")
		return false
	}

	result := PagesOfConfigurationSets{[]ConfigSetList{}}
	result.page = append(result.page, body)

	currentpage := body.Meta.CurrentPage
	totalpages := body.Meta.TotalPages

	Info.Printf("Read Total Pages: %d with %d rules\n", body.Meta.TotalPages, body.Meta.RecordCount)

	// iterate through pages collecting all rules
	for currentpage := currentpage + 1; currentpage <= totalpages; currentpage++ {

		Info.Printf("Reading page: %d out of %d\n", currentpage, totalpages)
		//set our API call
		apiCall := apiEndpoint + "/wafs/configuration_sets?page[number]=" + strconv.Itoa(currentpage)

		resp, err := resty.R().
			SetHeader("Accept", "application/vnd.api+json").
			SetHeader("Fastly-Key", apiKey).
			SetHeader("Content-Type", "application/vnd.api+json").
			Get(apiCall)

		//check if we had an issue with our call
		if err != nil {
			Error.Println("Error with API call: " + apiCall)
			Error.Println(resp.String())
			return false
		}

		//unmarshal the response and extract the service id
		body := ConfigSetList{}
		json.Unmarshal([]byte(resp.String()), &body)
		result.page = append(result.page, body)
	}

	for _, p := range result.page {
		for _, c := range p.Data {
			Info.Printf("- Configuration Set %s -  %s - Active: %t \n", c.ID, c.Attributes.Name, c.Attributes.Active)
		}
	}

	return true

}

// getRuleInfo function
func getRuleInfo(apiEndpoint, apiKey, ruleID string) Rule {
	rule := Rule{}
	//set our API call
	apiCall := apiEndpoint + "/wafs/rules?page[size]=10&page[number]=1&filter[rule_id]=" + ruleID

	resp, err := resty.R().
		SetHeader("Accept", "application/vnd.api+json").
		SetHeader("Fastly-Key", apiKey).
		SetHeader("Content-Type", "application/vnd.api+json").
		Get(apiCall)

	//check if we had an issue with our call
	if err != nil {
		Error.Println("Error with API call: " + apiCall)
		Error.Println(resp.String())
	}

	//unmarshal the response and extract the service id
	body := RuleList{}
	json.Unmarshal([]byte(resp.String()), &body)

	if len(body.Data) == 0 {
		Error.Println("No Fastly Rules found")
	}

	for _, r := range body.Data {
		rule = r
	}

	return rule
}

// getRules functions lists all rules for a WAFID and their status
func GetRules(apiEndpoint, apiKey, serviceID, wafID string) bool {
	//set our API call
	apiCall := apiEndpoint + "/service/" + serviceID + "/wafs/" + wafID + "/rule_statuses"

	resp, err := resty.R().
		SetHeader("Accept", "application/vnd.api+json").
		SetHeader("Fastly-Key", apiKey).
		SetHeader("Content-Type", "application/vnd.api+json").
		Get(apiCall)

	//check if we had an issue with our call
	if err != nil {
		Error.Println("Error with API call: " + apiCall)
		Error.Println(resp.String())
		return false
	}

	//unmarshal the response and extract the service id
	body := RuleList{}
	json.Unmarshal([]byte(resp.String()), &body)

	if len(body.Data) == 0 {
		Error.Println("No Fastly Rules found")
		return false
	}

	result := PagesOfRules{[]RuleList{}}
	result.page = append(result.page, body)

	currentpage := body.Meta.CurrentPage
	totalpages := body.Meta.TotalPages

	Info.Printf("Read Total Pages: %d with %d rules\n", body.Meta.TotalPages, body.Meta.RecordCount)

	// iterate through pages collecting all rules
	for currentpage := currentpage + 1; currentpage <= totalpages; currentpage++ {

		Info.Printf("Reading page: %d out of %d\n", currentpage, totalpages)
		//set our API call
		apiCall := apiEndpoint + "/service/" + serviceID + "/wafs/" + wafID + "/rule_statuses?page[number]=" + strconv.Itoa(currentpage)

		resp, err := resty.R().
			SetHeader("Accept", "application/vnd.api+json").
			SetHeader("Fastly-Key", apiKey).
			SetHeader("Content-Type", "application/vnd.api+json").
			Get(apiCall)

		//check if we had an issue with our call
		if err != nil {
			Error.Println("Error with API call: " + apiCall)
			Error.Println(resp.String())
			return false
		}

		//unmarshal the response and extract the service id
		body := RuleList{}
		json.Unmarshal([]byte(resp.String()), &body)
		result.page = append(result.page, body)
	}

	var log []Rule
	var disabled []Rule
	var block []Rule

	for _, p := range result.page {
		for _, r := range p.Data {
			switch r.Attributes.Status {
			case "log":
				log = append(log, r)
			case "block":
				block = append(block, r)
			case "disabled":
				disabled = append(disabled, r)
			}
		}
	}

	Info.Println("- Blocking Rules")
	for _, r := range block {
		info := getRuleInfo(apiEndpoint, apiKey, r.Attributes.ModsecRuleID)
		Info.Printf("- Rule ID: %s\tStatus: %s\tParanoia: %d\tPublisher: %s\tMessage: %s\n",
			r.Attributes.ModsecRuleID, r.Attributes.Status, info.Attributes.ParanoiaLevel,
			info.Attributes.Publisher, info.Attributes.Message)
	}

	Info.Println("- Logging Rules")
	for _, r := range log {
		info := getRuleInfo(apiEndpoint, apiKey, r.Attributes.ModsecRuleID)
		Info.Printf("- Rule ID: %s\tStatus: %s\tParanoia: %d\tPublisher: %s\tMessage: %s\n",
			r.Attributes.ModsecRuleID, r.Attributes.Status, info.Attributes.ParanoiaLevel,
			info.Attributes.Publisher, info.Attributes.Message)
	}

	Info.Println("- Disabled Rules")
	for _, r := range disabled {
		info := getRuleInfo(apiEndpoint, apiKey, r.Attributes.ModsecRuleID)
		Info.Printf("- Rule ID: %s\tStatus: %s\tParanoia: %d\tPublisher: %s\tMessage: %s\n",
			r.Attributes.ModsecRuleID, r.Attributes.Status, info.Attributes.ParanoiaLevel,
			info.Attributes.Publisher, info.Attributes.Message)
	}
	return true
}

// getAllRules function lists all the rules with in the Fastly API
func GetAllRules(apiEndpoint, apiKey, configID string) bool {

	if configID == "" {
		//set our API call
		apiCall := apiEndpoint + "/wafs/rules?page[number]=1"

		resp, err := resty.R().
			SetHeader("Accept", "application/vnd.api+json").
			SetHeader("Fastly-Key", apiKey).
			SetHeader("Content-Type", "application/vnd.api+json").
			Get(apiCall)

		//check if we had an issue with our call
		if err != nil {
			Error.Println("Error with API call: " + apiCall)
			Error.Println(resp.String())
			return false
		}

		//unmarshal the response and extract the service id
		body := RuleList{}
		json.Unmarshal([]byte(resp.String()), &body)

		if len(body.Data) == 0 {
			Error.Println("No Fastly Rules found")
			return false
		}

		result := PagesOfRules{[]RuleList{}}
		result.page = append(result.page, body)

		currentpage := body.Meta.CurrentPage
		totalpages := body.Meta.TotalPages

		Info.Printf("Read Total Pages: %d with %d rules\n", body.Meta.TotalPages, body.Meta.RecordCount)

		// iterate through pages collecting all rules
		for currentpage := currentpage + 1; currentpage <= totalpages; currentpage++ {

			Info.Printf("Reading page: %d out of %d\n", currentpage, totalpages)
			//set our API call
			apiCall := apiEndpoint + "/wafs/rules?page[number]=" + strconv.Itoa(currentpage)

			resp, err := resty.R().
				SetHeader("Accept", "application/vnd.api+json").
				SetHeader("Fastly-Key", apiKey).
				SetHeader("Content-Type", "application/vnd.api+json").
				Get(apiCall)

			//check if we had an issue with our call
			if err != nil {
				Error.Println("Error with API call: " + apiCall)
				Error.Println(resp.String())
				return false
			}

			//unmarshal the response and extract the service id
			body := RuleList{}
			json.Unmarshal([]byte(resp.String()), &body)
			result.page = append(result.page, body)
		}

		var owasp []Rule
		var fastly []Rule
		var trustwave []Rule

		for _, p := range result.page {
			for _, r := range p.Data {
				switch r.Attributes.Publisher {
				case "owasp":
					owasp = append(owasp, r)
				case "trustwave":
					trustwave = append(trustwave, r)
				case "fastly":
					fastly = append(fastly, r)
				}
			}
		}

		Info.Println("- OWASP Rules")
		for _, r := range owasp {
			Info.Printf("- Rule ID: %s\tParanoia: %d\tVersion: %s\tMessage: %s\n", r.ID, r.Attributes.ParanoiaLevel, r.Attributes.Version, r.Attributes.Message)
		}

		Info.Println("- Fastly Rules")
		for _, r := range fastly {
			Info.Printf("- Rule ID: %s\tParanoia: %d\tVersion: %s\tMessage: %s\n", r.ID, r.Attributes.ParanoiaLevel, r.Attributes.Version, r.Attributes.Message)
		}

		Info.Println("- Trustwave Rules")
		for _, r := range trustwave {
			Info.Printf("- Rule ID: %s\tParanoia: %d\tVersion: %s\tMessage: %s\n", r.ID, r.Attributes.ParanoiaLevel, r.Attributes.Version, r.Attributes.Message)
		}
	} else {

		//set our API call
		apiCall := apiEndpoint + "/wafs/rules?filter[configuration_set_id]=" + configID + "&page[number]=1"

		resp, err := resty.R().
			SetHeader("Accept", "application/vnd.api+json").
			SetHeader("Fastly-Key", apiKey).
			SetHeader("Content-Type", "application/vnd.api+json").
			Get(apiCall)

		//check if we had an issue with our call
		if err != nil {
			Error.Println("Error with API call: " + apiCall)
			Error.Println(resp.String())
			return false
		}

		//unmarshal the response and extract the service id
		body := RuleList{}
		json.Unmarshal([]byte(resp.String()), &body)

		if len(body.Data) == 0 {
			Error.Println("No Fastly Rules found")
			return false
		}

		result := PagesOfRules{[]RuleList{}}
		result.page = append(result.page, body)

		currentpage := body.Meta.CurrentPage
		totalpages := body.Meta.TotalPages

		Info.Printf("Read Total Pages: %d with %d rules\n", body.Meta.TotalPages, body.Meta.RecordCount)

		// iterate through pages collecting all rules
		for currentpage := currentpage + 1; currentpage <= totalpages; currentpage++ {

			Info.Printf("Reading page: %d out of %d\n", currentpage, totalpages)
			//set our API call
			apiCall := apiEndpoint + "/wafs/rules?filter[configuration_set_id]=" + configID + "&page[number]=" + strconv.Itoa(currentpage)

			resp, err := resty.R().
				SetHeader("Accept", "application/vnd.api+json").
				SetHeader("Fastly-Key", apiKey).
				SetHeader("Content-Type", "application/vnd.api+json").
				Get(apiCall)

			//check if we had an issue with our call
			if err != nil {
				Error.Println("Error with API call: " + apiCall)
				Error.Println(resp.String())
				return false
			}

			//unmarshal the response and extract the service id
			body := RuleList{}
			json.Unmarshal([]byte(resp.String()), &body)
			result.page = append(result.page, body)
		}

		var owasp []Rule
		var fastly []Rule
		var trustwave []Rule

		for _, p := range result.page {
			for _, r := range p.Data {
				switch r.Attributes.Publisher {
				case "owasp":
					owasp = append(owasp, r)
				case "trustwave":
					trustwave = append(trustwave, r)
				case "fastly":
					fastly = append(fastly, r)
				}
			}
		}

		Info.Println("- OWASP Rules")
		for _, r := range owasp {
			Info.Printf("- Rule ID: %s\tParanoia: %d\tVersion: %s\tMessage: %s\n", r.ID, r.Attributes.ParanoiaLevel, r.Attributes.Version, r.Attributes.Message)
		}

		Info.Println("- Fastly Rules")
		for _, r := range fastly {
			Info.Printf("- Rule ID: %s\tParanoia: %d\tVersion: %s\tMessage: %s\n", r.ID, r.Attributes.ParanoiaLevel, r.Attributes.Version, r.Attributes.Message)
		}

		Info.Println("- Trustwave Rules")
		for _, r := range trustwave {
			Info.Printf("- Rule ID: %s\tParanoia: %d\tVersion: %s\tMessage: %s\n", r.ID, r.Attributes.ParanoiaLevel, r.Attributes.Version, r.Attributes.Message)
		}

	}

	return true

}

// backupConfig function stores all rules, status, configuration set, and OWASP configuration locally
func BackupConfig(apiEndpoint, apiKey, serviceID, wafID string, client fastly.Client, bpath string) bool {

	//validate the output path
	d := filepath.Dir(bpath)
	if _, err := os.Stat(d); os.IsNotExist(err) {
		Error.Printf("Output path does not exist: %s\n", d)
		return false
	}

	//get all rules and their status
	//set our API call
	apiCall := apiEndpoint + "/service/" + serviceID + "/wafs/" + wafID + "/rule_statuses"

	resp, err := resty.R().
		SetHeader("Accept", "application/vnd.api+json").
		SetHeader("Fastly-Key", apiKey).
		SetHeader("Content-Type", "application/vnd.api+json").
		Get(apiCall)

	//check if we had an issue with our call
	if err != nil {
		Error.Println("Error with API call: " + apiCall)
		Error.Println(resp.String())
		return false
	}

	//unmarshal the response and extract the service id
	body := RuleList{}
	json.Unmarshal([]byte(resp.String()), &body)

	if len(body.Data) == 0 {
		Error.Println("No Fastly Rules found to back up")
		return false
	}

	result := PagesOfRules{[]RuleList{}}
	result.page = append(result.page, body)

	currentpage := body.Meta.CurrentPage
	perpage := body.Meta.PerPage
	totalpages := body.Meta.TotalPages

	Info.Printf("Backing up %d rules\n", body.Meta.RecordCount)

	// iterate through pages collecting all rules
	for currentpage := currentpage + 1; currentpage <= totalpages; currentpage++ {

		Info.Printf("Reading page: %d out of %d\n", currentpage, totalpages)
		//set our API call
		apiCall := fmt.Sprintf("%s/service/%s/wafs/%s/rule_statuses?page[size]=%d&page[number]=%d", apiEndpoint, serviceID, wafID, perpage, currentpage)

		resp, err := resty.R().
			SetHeader("Accept", "application/vnd.api+json").
			SetHeader("Fastly-Key", apiKey).
			SetHeader("Content-Type", "application/vnd.api+json").
			Get(apiCall)

		//check if we had an issue with our call
		if err != nil {
			Error.Println("Error with API call: " + apiCall)
			Error.Println(resp.String())
			return false
		}

		//unmarshal the response and extract the service id
		body := RuleList{}
		json.Unmarshal([]byte(resp.String()), &body)
		result.page = append(result.page, body)
	}

	var log []string
	var disabled []string
	var block []string

	for _, p := range result.page {
		for _, r := range p.Data {
			switch r.Attributes.Status {
			case "log":
				log = append(log, r.Attributes.ModsecRuleID)
			case "block":
				block = append(block, r.Attributes.ModsecRuleID)
			case "disabled":
				disabled = append(disabled, r.Attributes.ModsecRuleID)
			}
		}
	}

	//backup OWASP settings
	owasp, _ := client.GetOWASP(&fastly.GetOWASPInput{
		Service: serviceID,
		ID:      wafID,
	})

	if owasp.ID == "" {
		Error.Println("No OWASP Object to back up")
		return false
	}

	o := owaspSettings{
		AllowedHTTPVersions:              owasp.AllowedHTTPVersions,
		AllowedMethods:                   owasp.AllowedMethods,
		AllowedRequestContentType:        owasp.AllowedRequestContentType,
		AllowedRequestContentTypeCharset: owasp.AllowedRequestContentTypeCharset,
		ArgLength:                        owasp.ArgLength,
		ArgNameLength:                    owasp.ArgNameLength,
		CombinedFileSizes:                owasp.CombinedFileSizes,
		CriticalAnomalyScore:             owasp.CriticalAnomalyScore,
		CRSValidateUTF8Encoding:          owasp.CRSValidateUTF8Encoding,
		ErrorAnomalyScore:                owasp.ErrorAnomalyScore,
		HTTPViolationScoreThreshold:      owasp.HTTPViolationScoreThreshold,
		InboundAnomalyScoreThreshold:     owasp.InboundAnomalyScoreThreshold,
		LFIScoreThreshold:                owasp.LFIScoreThreshold,
		MaxFileSize:                      owasp.MaxFileSize,
		MaxNumArgs:                       owasp.MaxNumArgs,
		NoticeAnomalyScore:               owasp.NoticeAnomalyScore,
		ParanoiaLevel:                    owasp.ParanoiaLevel,
		PHPInjectionScoreThreshold:       owasp.PHPInjectionScoreThreshold,
		RCEScoreThreshold:                owasp.RCEScoreThreshold,
		RestrictedExtensions:             owasp.RestrictedExtensions,
		RestrictedHeaders:                owasp.RestrictedHeaders,
		RFIScoreThreshold:                owasp.RFIScoreThreshold,
		SessionFixationScoreThreshold:    owasp.SessionFixationScoreThreshold,
		SQLInjectionScoreThreshold:       owasp.SQLInjectionScoreThreshold,
		XSSScoreThreshold:                owasp.XSSScoreThreshold,
		TotalArgLength:                   owasp.TotalArgLength,
		WarningAnomalyScore:              owasp.WarningAnomalyScore,
	}

	//create a hash
	hasher := sha1.New()
	hasher.Write([]byte(serviceID + time.Now().String()))
	sha := hex.EncodeToString((hasher.Sum(nil)))

	//Safe Backup Object
	backup := Backup{
		ID:        sha,
		ServiceID: serviceID,
		Disabled:  disabled,
		Block:     block,
		Log:       log,
		Owasp:     o,
		Updated:   time.Now(),
	}

	buf := new(bytes.Buffer)
	if err := toml.NewEncoder(buf).Encode(backup); err != nil {
		Error.Println(err)
		return false
	}

	err = ioutil.WriteFile(bpath, buf.Bytes(), 0644)
	if err != nil {
		Error.Println(err)
		return false
	}

	Info.Printf("Bytes written: %d to %s\n", buf.Len(), bpath)
	return true
}

