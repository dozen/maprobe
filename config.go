package maprobe

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	mackerel "github.com/mackerelio/mackerel-client-go"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

type Config struct {
	location string

	APIKey string `yaml:"apikey"`

	Probes            []*ProbeDefinition `yaml:"probes"`
	PostProbedMetrics bool               `yaml:"post_probed_metrics"`

	Aggregates            []*AggregateDefinition `yaml:"aggregates"`
	PostAggregatedMetrics bool                   `yaml:"post_aggregated_metrics"`

	ProbeOnly *bool `yaml:"probe_only"` // deprecated
}

type ProbeDefinition struct {
	Service  string   `yaml:"service"`
	Role     string   `yaml:"role"`
	Roles    []string `yaml:"roles"`
	Statuses []string `yaml:"statuses"`

	Ping    *PingProbeConfig    `yaml:"ping"`
	TCP     *TCPProbeConfig     `yaml:"tcp"`
	HTTP    *HTTPProbeConfig    `yaml:"http"`
	Command *CommandProbeConfig `yaml:"command"`
}

func (pd *ProbeDefinition) GenerateProbes(host *mackerel.Host, client *mackerel.Client) []Probe {
	var probes []Probe

	if pingConfig := pd.Ping; pingConfig != nil {
		p, err := pingConfig.GenerateProbe(host)
		if err != nil {
			log.Printf("[error] cannot generate ping probe. HostID:%s Name:%s %s", host.ID, host.Name, err)
		} else {
			probes = append(probes, p)
		}
	}

	if tcpConfig := pd.TCP; tcpConfig != nil {
		p, err := tcpConfig.GenerateProbe(host)
		if err != nil {
			log.Printf("[error] cannot generate tcp probe. HostID:%s Name:%s %s", host.ID, host.Name, err)
		} else {
			probes = append(probes, p)
		}
	}

	if httpConfig := pd.HTTP; httpConfig != nil {
		p, err := httpConfig.GenerateProbe(host)
		if err != nil {
			log.Printf("[error] cannot generate http probe. HostID:%s Name:%s %s", host.ID, host.Name, err)
		} else {
			probes = append(probes, p)
		}
	}

	if commandConfig := pd.Command; commandConfig != nil {
		p, err := commandConfig.GenerateProbe(host, client)
		if err != nil {
			log.Printf("[error] cannot generate command probe. HostID:%s Name:%s %s", host.ID, host.Name, err)
		} else {
			probes = append(probes, p)
		}
	}

	return probes
}

func LoadConfig(location string) (*Config, error) {
	c := &Config{
		location:              location,
		APIKey:                os.Getenv("MACKEREL_APIKEY"),
		PostProbedMetrics:     true,
		PostAggregatedMetrics: true,
	}
	b, err := c.fetch()
	if err != nil {
		return nil, errors.Wrap(err, "load config failed")
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	c.initialize()
	return c, c.validate()
}

func (c *Config) initialize() {
	// role -> roles
	for _, pd := range c.Probes {
		if pd.Role != "" {
			pd.Roles = append(pd.Roles, pd.Role)
		}
	}
	for _, ad := range c.Aggregates {
		if ad.Role != "" {
			ad.Roles = append(ad.Roles, ad.Role)
		}
	}
}

func (c *Config) validate() error {
	if c.APIKey == "" {
		return errors.New("no API Key")
	}
	if o := c.ProbeOnly; o != nil {
		log.Println("[warn] configuration probe_only is not deprecated. use post_probed_metrics")
		c.PostProbedMetrics = !*o
	}

	for _, ag := range c.Aggregates {
		for _, mc := range ag.Metrics {
			for _, oc := range mc.Outputs {
				switch strings.ToLower(oc.Func) {
				case "sum":
					oc.calc = sum
				case "min":
					oc.calc = min
				case "max":
					oc.calc = max
				case "avg", "average":
					oc.calc = avg
				case "count":
					oc.calc = count
				default:
					log.Printf(
						"[warn] func %s is not available for outputs %s",
						oc.Func, mc.Name,
					)
				}
			}
		}
	}

	return nil
}

func (c *Config) fetch() ([]byte, error) {
	u, err := url.Parse(c.location)
	if err != nil {
		// file path
		return ioutil.ReadFile(c.location)
	}
	switch u.Scheme {
	case "http", "https":
		return fetchHTTP(u)
	case "s3":
		return fetchS3(u)
	default:
		// file
		return ioutil.ReadFile(u.Path)
	}
}

func (c *Config) String() string {
	b, _ := json.Marshal(c)
	return string(b)
}

func fetchHTTP(u *url.URL) ([]byte, error) {
	log.Println("[debug] fetching HTTP", u)
	resp, err := http.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return ioutil.ReadAll(resp.Body)
}

func fetchS3(u *url.URL) ([]byte, error) {
	log.Println("[debug] fetching S3", u)
	sess := session.Must(session.NewSession())
	downloader := s3manager.NewDownloader(sess)

	buf := &aws.WriteAtBuffer{}
	_, err := downloader.Download(buf, &s3.GetObjectInput{
		Bucket: aws.String(u.Host),
		Key:    aws.String(u.Path),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch from S3, %s", err)
	}
	return buf.Bytes(), nil
}

type AggregateDefinition struct {
	Service  string          `yaml:"service"`
	Role     string          `yaml:"role"`
	Roles    []string        `yaml:"roles"`
	Statuses []string        `yaml:"statuses"`
	Metrics  []*MetricConfig `yaml:"metrics"`
}

type MetricConfig struct {
	Name    string          `yaml:"name"`
	Outputs []*OutputConfig `yaml:"outputs"`
}

type OutputConfig struct {
	Func string `yaml:"func"`
	Name string `yaml:"name"`

	calc func([]float64) float64
}
