package hpprintersreceiver

import (
	"context"
	"errors"
	"hpprintersreceiver/internal/metadata"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configopaque"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/multierr"
	"go.uber.org/zap"
)

var (
	errClientNotInit = errors.New("client not initialized")
)

type hpprintersScraper struct {
	clients  []*http.Client
	cfg      *Config
	settings component.TelemetrySettings
	mb       *metadata.MetricsBuilder
}

type clientConfig struct {
	client   *http.Client
	ctx      context.Context
	endpoint string
	headers  configopaque.MapList
}

type usage struct {
	quantity  int64
	paperSize string
}

type cartridge struct {
	left  int64
	color metadata.AttributePrinterCartridgeColor
}

func makeRequest(config clientConfig, url string) (body io.ReadCloser, err error) {
	req, err := http.NewRequestWithContext(
		config.ctx,
		"GET",
		config.endpoint+url,
		http.NoBody,
	)
	if err != nil {
		return
	}

	// Add headers to the request
	for key, value := range config.headers.Iter {
		req.Header.Set(key, value.String()) // Convert configopaque.String to string
	}

	resp, err := config.client.Do(req)
	if err != nil {
		return
	}

	// Read response body and return to client
	body = resp.Body
	if body != nil {
		err = errors.New("no response from server")
	}

	return
}

func makeDoc(config clientConfig, url string) (doc *goquery.Document, err error) {
	page, err := makeRequest(config, url)
	if err != nil {
		return
	}

	doc, err = goquery.NewDocumentFromReader(page)
	if err != nil {
		return
	}

	return
}

func scrapeDeviceInfo(config clientConfig) (deviceModelName string, deviceModelIdentifier string, deviceId string, err error) {
	doc, err := makeDoc(config, "hp/device/DeviceInformation/View")
	if err != nil {
		return
	}

	deviceModelName = doc.Find("#ProductName").Text()
	deviceModelIdentifier = doc.Find("#DeviceModel").Text()
	deviceId = doc.Find("#DeviceSerialNumber").Text()

	return
}

func scrapeHostInfo(config clientConfig) (hostName string, hostIp string, err error) {
	doc, err := makeDoc(config, "tcp_summary.htm")
	if err != nil {
		return
	}

	hostName = strings.Split(doc.Find("#HostName").Text(), " : ")[1]
	hostIpV4 := doc.Find("#addrta tbody > tr:nth-child(1) > td:nth-child(2)").Text()
	hostIpV6 := doc.Find("#IPv6AddrLst tbody > tr > td:nth-child(1)").Text()
	hostIp = hostIpV4 + "," + hostIpV6

	return
}

func scrapeCartridgeInfo(config clientConfig) (cartridges []cartridge, err error) {
	doc, err := makeDoc(config, "hp/device/InternalPages/Index?id=SuppliesStatus")
	if err != nil {
		return
	}

	doc.Find(".toner.consumable-block-black").Each(func(_ int, s *goquery.Selection) {
		percentage := s.Find(".percentage").Text()
		idx := strings.Index(percentage, "%")

		if idx == -1 {
			return
		}

		percentage = percentage[:idx]
		intPercentage, err := strconv.ParseInt(percentage, 10, 64)
		if err != nil {
			return
		}

		cartridges = append(cartridges, cartridge{
			left:  intPercentage,
			color: metadata.AttributePrinterCartridgeColorBlack,
		})
	})

	return
}

func scrapeUsageInfo(config clientConfig) (printUsage []usage, copyUsage []usage, scanUsage []usage, err error) {
	doc, err := makeDoc(config, "hp/device/InternalPages/Index?id=UsagePage")
	if err != nil {
		return
	}

	printUsage = scrapeUsageData(doc, "[id^=UsagePage.ImpressionsByMediaSizeTable]:nth-child(1) tbody > tr")
	copyUsage = scrapeUsageData(doc, "[id^=UsagePage.ImpressionsByMediaSizeTable]:nth-child(2) tbody > tr")
	printUsage = scrapeUsageData(doc, "#UsagePage.ScanBySizeTable tbody > tr")

	return
}

func scrapeUsageData(document *goquery.Document, selector string) (data []usage) {
	document.Find(selector).Each(func(_ int, s *goquery.Selection) {
		paperSize := s.Find("td:first-child").Text()
		idx := strings.Index(paperSize, " (")

		if idx == -1 {
			return
		}

		paperSize = paperSize[:idx]
		quantity := s.Find("td:last-child").Text()
		intQuantity, err := strconv.ParseInt(quantity, 10, 64)
		if err != nil {
			return
		}

		data = append(data, usage{
			quantity:  intQuantity,
			paperSize: paperSize,
		})
	})

	return
}

// start initializes the scraper by creating HTTP clients for each endpoint.
func (h *hpprintersScraper) start(ctx context.Context, host component.Host) (err error) {
	var expandedTargets []*targetConfig

	for _, target := range h.cfg.Targets {
		if target.Timeout == 0 {
			// Set a reasonable timeout to prevent hanging requests
			target.Timeout = 30 * time.Second
		}

		// Create a unified list of endpoints
		var allEndpoints []string
		if len(target.Endpoints) > 0 {
			allEndpoints = append(allEndpoints, target.Endpoints...) // Add all endpoints
		}
		if target.Endpoint != "" {
			allEndpoints = append(allEndpoints, target.Endpoint) // Add single endpoint
		}

		// Process each endpoint in the unified list
		for _, endpoint := range allEndpoints {
			client, clientErr := target.ToClient(ctx, host.GetExtensions(), h.settings)
			if clientErr != nil {
				h.settings.Logger.Error("failed to initialize HTTP client", zap.String("endpoint", endpoint), zap.Error(clientErr))
				err = multierr.Append(err, clientErr)
				continue
			}

			// Clone the target and assign the specific endpoint
			targetClone := *target
			targetClone.Endpoint = endpoint

			h.clients = append(h.clients, client)
			expandedTargets = append(expandedTargets, &targetClone) // Add the cloned target to expanded targets
		}
	}

	h.cfg.Targets = expandedTargets // Replace targets with expanded targets
	return err
}

func (h *hpprintersScraper) scrape(ctx context.Context) (pmetric.Metrics, error) {
	if len(h.clients) == 0 {
		return pmetric.NewMetrics(), errClientNotInit
	}

	rb := h.mb.NewResourceBuilder()

	var wg sync.WaitGroup
	wg.Add(len(h.clients))
	var mux sync.Mutex

	for idx, client := range h.clients {
		go func(targetClient *http.Client, targetIndex int) {
			defer wg.Done()

			now := pcommon.NewTimestampFromTime(time.Now())
			config := clientConfig{
				client:   targetClient,
				ctx:      ctx,
				endpoint: h.cfg.Targets[targetIndex].Endpoint,
				headers:  h.cfg.Targets[targetIndex].Headers,
			}

			deviceModelName, deviceModelIdentifier, deviceId, err := scrapeDeviceInfo(config)
			if err != nil {
				h.settings.Logger.Error("Failed to scrape device info", zap.Error(err))
				return
			}

			hostName, hostIp, err := scrapeHostInfo(config)
			if err != nil {
				h.settings.Logger.Error("Failed to scrape host info", zap.Error(err))
				return
			}

			cartridges, err := scrapeCartridgeInfo(config)
			if err != nil {
				h.settings.Logger.Error("Failed to scrape cartridge info", zap.Error(err))
				return
			}

			printUsage, copyUsage, scanUsage, err := scrapeUsageInfo(config)
			if err != nil {
				h.settings.Logger.Error("Failed to scrape usage info", zap.Error(err))
				return
			}

			mux.Lock()

			rb.SetDeviceID(deviceId)
			rb.SetDeviceManufacturerHP()
			rb.SetDeviceModelIdentifier(deviceModelIdentifier)
			rb.SetDeviceModelName(deviceModelName)
			rb.SetHostIP(hostIp)
			rb.SetHostName(hostName)
			r := rb.Emit()

			for _, cartridge := range cartridges {
				h.mb.RecordPrinterCartridgeLeftDataPoint(now, cartridge.left, cartridge.color)
			}
			for _, usage := range printUsage {
				h.mb.RecordPrinterUsagePrintDataPoint(now, usage.quantity, usage.paperSize)
			}
			for _, usage := range copyUsage {
				h.mb.RecordPrinterUsageCopyDataPoint(now, usage.quantity, usage.paperSize)
			}
			for _, usage := range scanUsage {
				h.mb.RecordPrinterUsageScanDataPoint(now, usage.quantity, usage.paperSize)
			}
			h.mb.EmitForResource(metadata.WithResource(r))

			mux.Unlock()
		}(client, idx)
	}

	wg.Wait()

	metrics := h.mb.Emit()

	return metrics, nil
}

func newScraper(conf *Config, settings receiver.Settings) *hpprintersScraper {
	return &hpprintersScraper{
		cfg:      conf,
		settings: settings.TelemetrySettings,
		mb:       metadata.NewMetricsBuilder(conf.MetricsBuilderConfig, settings),
	}
}
