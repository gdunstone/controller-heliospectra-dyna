package main

import (
	"encoding/xml"
	"flag"
	"fmt"
	"github.com/appf-anu/chamber-tools"
	"github.com/mdaffin/go-telegraf"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	errLog     *log.Logger
)

var (
	noMetrics, dummy, loopFirstDay            bool
	address		  							  string
	statusUrl, intensityUrl					  *url.URL
	multiplier                                float64
	conditionsPath, hostTag, groupTag, didTag string
	interval                                  time.Duration
)

const (
	matchFloatExp   = `[-+]?\d*\.\d+|\d+`
	matchIntsExp    = `\b(\d+)\b`
	matchOKExp      = `OK`
	matchStringsExp = `\b(\w+)\b`
)

// TsRegex is a regexp to find a timestamp within a filename
var /* const */ matchFloat = regexp.MustCompile(matchFloatExp)
var /* const */ matchInts = regexp.MustCompile(matchIntsExp)
var /* const */ matchOK = regexp.MustCompile(matchOKExp)
var /* const */ matchStrings = regexp.MustCompile(matchStringsExp)

const (
	// it is extremely unlikely (see. impossible) that we will be measuring a humidity of 214,748,365 %RH or a
	// temperature of -340,282,346,638,528,859,811,704,183,484,516,925,440°C until we invent some new physics, so until
	// then, I will use these values as the unset or null values for HumidityTarget and TemperatureTarget
	nullTargetInt   = math.MinInt32
	nullTargetFloat = -math.MaxFloat32
)


var usage = func() {
	use := `
usage of %s:
flags:
	-no-metrics: don't send metrics to telegraf
	-dummy: don't control the chamber, only collect metrics (this is implied by not specifying a conditions file
	-conditions: conditions to use to run the chamber
	-interval: what interval to run conditions/record metrics at, set to 0s to read 1 metric and exit. (default=10m)

examples:
	collect data on 192.168.1.3  and output the errors to GC03-error.log and record the output to GC03.log
	%s -dummy 192.168.1.3 2>> GC03-error.log 1>> GC03.log

	run conditions on 192.168.1.3  and output the errors to GC03-error.log and record the output to GC03.log
	%s -conditions GC03-conditions.csv -dummy 192.168.1.3 2>> GC03-error.log 1>> GC03.log

quirks:
channels are sequentially numbered as such in conditions file:

	s7:
		channel-1 400nm
		channel-2 420nm
		channel-3 450nm
		channel-4 530nm
		channel-5 630nm
		channel-6 660nm
		channel-7 735nm
	s10:
		channel-1 370nm
		channel-2 400nm
		channel-3 420nm
		channel-4 450nm
		channel-5 530nm
		channel-6 620nm
		channel-7 660nm
		channel-8 735nm
		channel-9 850nm
		channel-10 6500k
	dyna:
		channel-1 380nm
		channel-2 400nm
		channel-3 420nm
		channel-4 450nm
		channel-5 530nm
		channel-6 620nm
		channel-7 660nm
		channel-8 735nm
		channel-9 5700K
`
	fmt.Printf(use, os.Args[0], os.Args[0], os.Args[0])
}

var WavelengthsS7 = []string{
	"400nm",
	"420nm",
	"450nm",
	"530nm",
	"630nm",
	"660nm",
	"735nm",
}


var WavelengthsS10 = []string{
	"370nm",
	"400nm",
	"420nm",
	"450nm",
	"530nm",
	"620nm",
	"660nm",
	"735nm",
	"850nm",
	"6500k",
}


var WavelengthsDyna = []string{
	"380nm",
	"400nm",
	"420nm",
	"450nm",
	"530nm",
	"620nm",
	"660nm",
	"735nm",
	"5700K",
}


func TrimSuffix(s, suffix string) string {
	if strings.HasSuffix(s, suffix) {
		s = s[:len(s)-len(suffix)]
	}
	return s
}

type XMLRepresentation struct {
	LightTime string `xml:"a"`
	ScheduleStatus string `xml:"b"`
	LightStatus string `xml:"c"`
	Uptime string `xml:"d"`
	LastChangeTime string `xml:"e"`
	LastChangeReason string `xml:"f"`
	LastChangeIp string `xml:"g"`
	LastChangeType string `xml:"h"`
	PanelTemperatures string `xml:"i"`
	Intensities string `xml:"j"`
	ControlMode string `xml:"m"`
	UIValues1 string `xml:"n"`
	UIValues2 string `xml:"o"`
	NTPInfo string `xml:"q"`
}

type LightStatus struct {
	LightTime time.Time
	ScheduleStatus bool
	LightStatus bool
	Uptime time.Duration
	LastChangeTime time.Time
	LastChangeReason string
	LastChangeIp string
	LastChangeType string
	PanelTemperatureC []float64
	Intensities []int64
	TargetIntensities []int64
	ControlMode string
	UILightsOnAtPowerUp bool
	UIStatusIndicatorLed bool
	UIScheduleLockOn bool
	UIScheduleLockMessage string
	UIScheduleLockPassword string
	NTPStatus bool
	NTPAddress string
	TimeZoneOffset string
	ExecutedTimepoint bool
}

func (lightStatus *LightStatus) Unmarshal(data []byte) error  {
	xmlrep := XMLRepresentation{}
	err := xml.Unmarshal(data, &xmlrep)
	if err != nil {
		errLog.Printf("error decoding status.xml: %v\n", err)
		return err
	}

	if xmlrep.LightTime != ""{
		//  light time
		lightTime, err := time.Parse("2006:01:02:15:04:05", xmlrep.LightTime)
		if err != nil{
			errLog.Printf("error decoding status.xml: %v\n", err)
		} else {
			lightStatus.LightTime = lightTime
		}
	}

	if xmlrep.ScheduleStatus != ""{
		// schedule status
		if xmlrep.ScheduleStatus  == "Running" {
			lightStatus.ScheduleStatus = true
		} else {
			lightStatus.ScheduleStatus = false
		}
	}

	if xmlrep.LightStatus != ""{
		// light status
		if xmlrep.LightStatus == "OK" {
			lightStatus.LightStatus = true
		} else {
			lightStatus.LightStatus = false
		}
	}

	if xmlrep.Uptime != ""{
		// uptime
		uptimeStr := strings.Replace(xmlrep.Uptime,  " ", "", -1)
		uptimeStrSplit := strings.Split(uptimeStr, "d")
		uptimeDuration, err := time.ParseDuration(uptimeStrSplit[len(uptimeStrSplit)-1])
		if err != nil{
			errLog.Printf("error decoding status.xml: %v\n", err)
		}else {
			if len(uptimeStrSplit) > 1{
				if durationDays, err := strconv.ParseInt(uptimeStrSplit[0], 10, 64); err != nil{
					errLog.Printf("error decoding status.xml: %v\n", err)
					uptimeDuration = -1 // we error so set duration to -1 to avoid incomplete value
				}else{
					hours := 24 * durationDays
					uptimeDuration += time.Duration(hours) * time.Hour
				}
			}
			if uptimeDuration > 0 {
				lightStatus.Uptime = uptimeDuration
			}
		}
	}

	if xmlrep.LastChangeTime != ""{
		// last changed time
		lastChangedTime, err := time.Parse("2006-01-02   15:04:05", xmlrep.LastChangeTime)
		if err != nil{
			errLog.Printf("error decoding status.xml: %v\n", err)
		} else {
			lightStatus.LastChangeTime = lastChangedTime
		}
	}

	lightStatus.LastChangeReason = xmlrep.LastChangeReason
	lightStatus.LastChangeIp = xmlrep.LastChangeIp
	lightStatus.LastChangeType = xmlrep.LastChangeType

	if xmlrep.PanelTemperatures != ""{
		// panel temperatures
		temperatureValuesStr := TrimSuffix(xmlrep.PanelTemperatures, ",")
		temperatureValues := strings.Split(temperatureValuesStr, ",")
		for _, tempStr := range temperatureValues {
			tval := strings.Split(tempStr, ":")
			tempStr = tval[len(tval)-1]
			tempUnit := tempStr[len(tempStr)-1:]
			tempValueStr := tempStr[:len(tempStr)-2]
			tempValue, err := strconv.ParseFloat(tempValueStr, 10)
			if err != nil{
				errLog.Printf("error decoding status.xml: %v\n", err)
			}else {
				// if temperature is in freedom units, convert to something that is useful in the real world
				if tempUnit == "F" {
					tempValue = 5.0/9.0 * (tempValue - 32.0)
				}
				lightStatus.PanelTemperatureC = append(lightStatus.PanelTemperatureC, tempValue)
			}
		}
	}
	if xmlrep.Intensities != ""{
		// light intensities
		intensityValuesStr := TrimSuffix(xmlrep.Intensities, ",")
		intensityValues := strings.Split(intensityValuesStr, ",")
		for _, intStr := range intensityValues {
			ival := strings.Split(intStr, ":")
			intStr = ival[len(ival)-1]
			intensityValue, err := strconv.ParseInt(intStr, 10, 64)

			if err != nil{
				errLog.Printf("error decoding status.xml: %v\n", err)
				// if intensity has ANY error, clear the intensity slice and dont try and parse any more intensity values
				lightStatus.Intensities = nil
				break
			}else {
				lightStatus.Intensities = append(lightStatus.Intensities, intensityValue)
			}
		}
	}
	// ignore 10, and 11.
	lightStatus.ControlMode = xmlrep.ControlMode

	if xmlrep.UIValues1 != ""{
		uiValues1 := make([]string, 3)

		copy(uiValues1, strings.Split(xmlrep.UIValues1, ":"))
		// uiValues[0] here is the temperature unit, we dont need this.

		if uiValues1[1] == "on"{
			lightStatus.UILightsOnAtPowerUp = true
		}else if uiValues1[1] == "off"{
			lightStatus.UILightsOnAtPowerUp = false
		}

		// for some reason this value is actually indicated true by the string "normal" instead of "on", wtf.
		if uiValues1[2] == "normal" {
			lightStatus.UIStatusIndicatorLed = true
		}else if uiValues1[2] == "off" {
			lightStatus.UIStatusIndicatorLed = false
		}
	}

	if xmlrep.UIValues2 != "" {
		uiValues2 := make([]string, 3)
		copy(uiValues2, strings.Split(xmlrep.UIValues2, ":"))
		if uiValues2[0] == "on" {
			lightStatus.UIScheduleLockOn = true
		}else if uiValues2[0] == "off" {
			lightStatus.UIScheduleLockOn = false
		}
		if uiValues2[1] != ""{
			lightStatus.UIScheduleLockMessage = uiValues2[1]
		}
		if uiValues2[2] != ""{
			lightStatus.UIScheduleLockPassword = uiValues2[2]
		}
	}

	// ignore 15
	if xmlrep.NTPInfo != ""{
		ntpValues := make([]string, 3)
		// for some reason ntp info is comma-space separated <shrug>
		copy(ntpValues, strings.Split(xmlrep.NTPInfo, ", "))
		if ntpValues[0] == "on" {
			lightStatus.NTPStatus = true
		} else if ntpValues[0] == "off" {
			lightStatus.NTPStatus = false
		}
		if ntpValues[1] != "" {
			lightStatus.NTPAddress = ntpValues[1]
		}
		if ntpValues[2] != "" {
			lightStatus.TimeZoneOffset = ntpValues[2]
		}
	}
	// we arent going to bother with the wifi settings.
	return nil
}


func GetLightStatus() (*LightStatus, error) {
	v := new(LightStatus)
	resp, err := http.Get(statusUrl.String())
	if err != nil{
		return nil, err
	}
	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	err = v.Unmarshal(data)
	if err != nil {
		return nil, err
	}
	return v, nil
}


func setMany(values []int64) (err error) {
	delim := ":"
	intensityQueryStringValues := strings.Trim(strings.Replace(fmt.Sprint(values), " ", delim, -1), "[]")
	q := url.Values{}
	q.Set("int", intensityQueryStringValues)
	intensityUrl.RawQuery = q.Encode()
	_, err = http.Get(intensityUrl.String())
	return err
}

// runStuff, should send values and write metrics.
// returns true if program should continue, false if program should retry
func runStuff(point *chamber_tools.TimePoint) bool {

	status, err := GetLightStatus()
	if err != nil {
		// should retry in 5 seconds because we couldn't contact the light.
		time.Sleep(5)
		return false
	}
	wavelengths := make([]string, 0)
	switch len(status.Intensities) {
	case len(WavelengthsDyna):
		wavelengths = WavelengthsDyna
	case len(WavelengthsS10):
		wavelengths = WavelengthsS10
	case len(WavelengthsS7):
		wavelengths = WavelengthsS7
	default:
		errLog.Printf("got incorrect number of intensities from device: %d\n", len(status.Intensities))
		return false
	}

	if len(wavelengths) != len(point.Channels){
		errLog.Printf("timepoint/device %d/%d channel number mismatch, ignoring timepoint\n", len(point.Channels), len(status.Intensities))
		return true
	}

	status.TargetIntensities = make([]int64, len(point.Channels))
	for idx, targetIntensity := range point.Channels {
		// multiply all the channel values by the multiplier.
		// none of the heliospectras accept values over 1000, so clamp
		if targetIntensity == chamber_tools.NullTargetFloat64 || targetIntensity < 0 {
			status.TargetIntensities[idx] = status.Intensities[idx]
			continue
		}else{
			status.TargetIntensities[idx] = int64(chamber_tools.Clamp(int(targetIntensity * multiplier), 0, 1000))
		}
	}

	err = setMany(status.TargetIntensities)
	if err != nil {
		errLog.Println(err)
		return false
	}
	status.ExecutedTimepoint = true
	errLog.Printf("ran %s %+v", point.Datetime.Format(time.RFC3339), status.TargetIntensities)

	for x := 0; x < 5; x++ {
		if err := writeMetrics(status); err != nil {
			errLog.Println(err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		break
	}
	return true
}

func writeMetrics(status *LightStatus) error {
	telegrafHost := "telegraf:8092"
	if tthost := os.Getenv("TELEGRAF_HOST"); tthost != "" {
		telegrafHost = tthost
	}

	telegrafClient, err := telegraf.NewUDP(telegrafHost)
	if err != nil {
		errLog.Println(err)
		return err
	}
	defer telegrafClient.Close()

	m := telegraf.NewMeasurement("heliospectra2")

	va := reflect.ValueOf(&status).Elem()
	for i := 0; i < va.NumField(); i++ {
		chamber_tools.DecodeStructFieldToMeasurement(&m, va, i)
	}

	if hostTag != "" {
		m.AddTag("host", hostTag)
	}
	if groupTag != "" {
		m.AddTag("group", groupTag)
	}
	if didTag != "" {
		m.AddTag("did", didTag)
	}

	return telegrafClient.Write(m)
}


func init() {
	var err error
	hostname := os.Getenv("NAME")

	if address = os.Getenv("ADDRESS"); address == "" {
		address = flag.Arg(0)
		u, err := url.Parse(address)
		if err != nil {
			panic(err)
		}
		u.Scheme = "http"
		u.Path = "/"
		u.RawQuery = ""

		statusUrl, err = url.Parse(u.String())
		if err != nil {
			panic(err)
		}
		statusUrl.Path = "/status.xml"

		intensityUrl, err = url.Parse(u.String())
		if err != nil {
			panic(err)
		}
		intensityUrl.Path = "/intensity.cgi"
		intensityUrl.RawQuery = ""
	}

	errLog = log.New(os.Stderr, "[heliospectra] ", log.Ldate|log.Ltime|log.Lshortfile)
	// get the local zone and offset

	flag.Usage = usage
	flag.BoolVar(&noMetrics, "no-metrics", false, "dont collect metrics")
	if tempV := strings.ToLower(os.Getenv("NO_METRICS")); tempV != "" {
		if tempV == "true" || tempV == "1" {
			noMetrics = true
		} else {
			noMetrics = false
		}
	}

	flag.BoolVar(&dummy, "dummy", false, "dont send conditions to light")
	if tempV := strings.ToLower(os.Getenv("DUMMY")); tempV != "" {
		if tempV == "true" || tempV == "1" {
			dummy = true
		} else {
			dummy = false
		}
	}

	flag.BoolVar(&loopFirstDay, "loop", false, "loop over the first day")
	if tempV := strings.ToLower(os.Getenv("LOOP")); tempV != "" {
		if tempV == "true" || tempV == "1" {
			loopFirstDay = true
		} else {
			loopFirstDay = false
		}
	}

	flag.StringVar(&hostTag, "host-tag", hostname, "host tag to add to the measurements")
	if tempV := os.Getenv("HOST_TAG"); tempV != "" {
		hostTag = tempV
	}

	flag.StringVar(&groupTag, "group-tag", "nonspc", "host tag to add to the measurements")
	if tempV := os.Getenv("GROUP_TAG"); tempV != "" {
		groupTag = tempV
	}

	flag.StringVar(&didTag, "did-tag", "", "deliverable id tag")
	if tempV := os.Getenv("DID_TAG"); tempV != "" {
		didTag = tempV
	}

	flag.StringVar(&conditionsPath, "conditions", "", "conditions file to")
	if tempV := os.Getenv("CONDITIONS_FILE"); tempV != "" {
		conditionsPath = tempV
	}
	flag.DurationVar(&interval, "interval", time.Minute*10, "interval to record metrics at")
	if tempV := os.Getenv("INTERVAL"); tempV != "" {
		interval, err = time.ParseDuration(tempV)
		if err != nil {
			errLog.Println("Couldnt parse interval from environment")
			errLog.Println(err)
		}
	}
	flag.Float64Var(&multiplier, "multiplier", 10.0, "multiplier for the light")
	if tempV := os.Getenv("MULTIPLIER"); tempV != "" {
		multiplier, err = strconv.ParseFloat(tempV, 64)
		if err != nil {
			errLog.Println("Couldnt parse multiplier from environment")
			errLog.Println(err)
		}
	}
	flag.Parse()

	if noMetrics && dummy {
		errLog.Println("dummy and no-metrics specified, nothing to do.")
		os.Exit(1)
	}
	if conditionsPath != "" && !dummy {
		chamber_tools.InitIndexConfig(errLog, conditionsPath)
	}
	errLog.Printf("hostTag: \t%s\n", hostTag)
	errLog.Printf("groupTag: \t%s\n", groupTag)
	errLog.Printf("address: \t%s\n", address)
	errLog.Printf("file: \t%s\n", conditionsPath)
	errLog.Printf("interval: \t%s\n", interval)
}

func main() {
	if !noMetrics && (conditionsPath == "" || dummy) {
		runMetrics := func() {
			status, err := GetLightStatus()
			if err != nil {
				errLog.Println(err)
				return
			}
			for x := 0; x < 5; x++ {
				if err := writeMetrics(status); err != nil {
					errLog.Println(err)
					time.Sleep(200 * time.Millisecond)
					continue
				}
				break
			}
		}

		runMetrics()

		ticker := time.NewTicker(interval)
		go func() {
			for range ticker.C {
				runMetrics()
			}
		}()
		select {}
	}

	if conditionsPath != "" && !dummy {
		chamber_tools.RunConditions(errLog, runStuff, conditionsPath, loopFirstDay)
	}
}
