package fc2hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gocolly/colly/v2"
	"golang.org/x/net/html"
	"golang.org/x/text/language"

	"github.com/metatube-community/metatube-sdk-go/common/parser"
	"github.com/metatube-community/metatube-sdk-go/model"
	"github.com/metatube-community/metatube-sdk-go/provider"
	"github.com/metatube-community/metatube-sdk-go/provider/fc2/fc2util"
	"github.com/metatube-community/metatube-sdk-go/provider/internal/scraper"
)

var (
	_ provider.MovieProvider = (*FC2HUB)(nil)
	_ provider.MovieSearcher = (*FC2HUB)(nil)
	_ provider.ConfigSetter  = (*FC2HUB)(nil)
)

const (
	Name     = "fc2hub"
	Priority = 1000 - 1
)

const (
	baseURL   = "https://javten.com/"
	movieURL  = "https://javten.com/video/%s/id%s/%s"
	searchURL = "https://javten.com/search?kw=%s"

	defaultCookieURL = "http://192.168.2.24:8000/cookies"
	cookieOrigin     = "https://javten.com/"
	cookieSessionTTL = 29 * time.Minute
	defaultTimeout   = time.Minute
)

type FC2HUB struct {
	*scraper.Scraper

	scrapeMu           sync.Mutex
	cookieURL          string
	cookieClient       *http.Client
	cookieSession      *cfCookieSession
	managedCookieNames map[string]struct{}
	now                func() time.Time
}

func New() *FC2HUB {
	return &FC2HUB{
		Scraper:            scraper.NewDefaultScraper(Name, baseURL, Priority, language.Japanese),
		cookieURL:          defaultCookieURL,
		cookieClient:       &http.Client{Timeout: defaultTimeout},
		managedCookieNames: make(map[string]struct{}),
		now:                time.Now,
	}
}

type cfCookieSession struct {
	cookies   map[string]string
	userAgent string
	expiresAt time.Time
}

type cfCookieResponse struct {
	Cookies   map[string]string `json:"cookies"`
	UserAgent string            `json:"user_agent"`
}

func (fc2hub *FC2HUB) SetRequestTimeout(timeout time.Duration) {
	fc2hub.scrapeMu.Lock()
	defer fc2hub.scrapeMu.Unlock()
	fc2hub.Scraper.SetRequestTimeout(timeout)
	fc2hub.cookieClient.Timeout = timeout
}

func (fc2hub *FC2HUB) SetConfig(config provider.Config) error {
	if !config.Has("cf_cookie_url") {
		return nil
	}
	cookieURL, err := config.GetString("cf_cookie_url")
	if err != nil {
		return fmt.Errorf("get cf_cookie_url: %w", err)
	}
	parsed, err := url.Parse(cookieURL)
	if err != nil || parsed == nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("cf_cookie_url must be an absolute http or https URL")
	}

	fc2hub.scrapeMu.Lock()
	defer fc2hub.scrapeMu.Unlock()
	fc2hub.cookieURL = parsed.String()
	fc2hub.cookieSession = nil
	fc2hub.clearManagedCookies()
	return nil
}

func (fc2hub *FC2HUB) GetMovieInfoByID(id string) (info *model.MovieInfo, err error) {
	ss := strings.SplitN(id, "-", 2)
	if len(ss) != 2 {
		return nil, provider.ErrInvalidID
	}
	const padding = "%20" // use padding to fix weird colly trailing path issue.
	return fc2hub.GetMovieInfoByURL(fmt.Sprintf(movieURL, ss[0], ss[1], padding))
}

func (fc2hub *FC2HUB) ParseMovieIDFromURL(rawURL string) (string, error) {
	homepage, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if ss := regexp.MustCompile(`/video/(\d+)/id(\d+)`).FindStringSubmatch(homepage.Path); len(ss) == 3 {
		return fmt.Sprintf("%s-%s", ss[1], ss[2]), nil
	}
	return "", provider.ErrInvalidURL
}

func (fc2hub *FC2HUB) GetMovieInfoByURL(rawURL string) (info *model.MovieInfo, err error) {
	if rawURL, err = fc2hub.normalizeMovieURL(rawURL); err != nil {
		return
	}

	fc2hub.scrapeMu.Lock()
	defer fc2hub.scrapeMu.Unlock()

	c := fc2hub.ClonedCollector()
	session := fc2hub.bootstrapCookies(rawURL)
	fc2hub.applyCookieSession(c, session)
	return fc2hub.getMovieInfoByURL(rawURL, c)
}

func (fc2hub *FC2HUB) getMovieInfoByURL(rawURL string, c *colly.Collector) (info *model.MovieInfo, err error) {
	id, err := fc2hub.ParseMovieIDFromURL(rawURL)
	if err != nil {
		return
	}

	info = &model.MovieInfo{
		ID:            id, // Dual-ID (id+number)
		Provider:      fc2hub.Name(),
		Homepage:      rawURL,
		Actors:        []string{},
		PreviewImages: []string{},
		Genres:        []string{},
	}

	// Allow redirecting, for cases like http -> https
	c.SetRedirectHandler(func(req *http.Request, via []*http.Request) error {
		return nil
	})

	// Title
	c.OnXML(`//*[@id="content"]/div/div[2]/div[1]/div[1]/div[2]/h1`, func(e *colly.XMLElement) {
		info.Title = strings.TrimSpace(e.Text)
	})

	// Summary
	c.OnXML(`//*[@id="content"]/div/div[2]/div[1]/div[1]/div[2]/div[2]/div`, func(e *colly.XMLElement) {
		info.Summary = strings.TrimSpace(e.Text)
	})

	// Number
	c.OnXML(`//*[@id="content"]/div/div[2]/div[1]/div[1]/div[2]/div[1]/div[2]/h1`, func(e *colly.XMLElement) {
		if num := fc2util.ParseNumber(strings.TrimSpace(e.Text)); num != "" {
			info.Number = fmt.Sprintf("FC2-%s", num)
		}
	})

	// Genres
	c.OnXML(`//*[@id="content"]/div/div[2]/div[1]/div[1]/div[2]/p/a`, func(e *colly.XMLElement) {
		if genre := strings.TrimSpace(e.Text); genre != "" {
			info.Genres = append(info.Genres, genre)
		}
	})

	// Maker
	c.OnXML(`//*[@id="content"]/div/div[2]/div[1]/div[3]/div/div[2]/div/div[2]`, func(e *colly.XMLElement) {
		// info.Maker = strings.TrimSpace(strings.Split(e.Text, "\n")[0])
		for n := e.DOM.(*html.Node).FirstChild; n != nil; n = n.NextSibling {
			if n.Type == html.TextNode {
				info.Maker = strings.TrimSpace(n.Data)
				break
			}
		}
	})

	// Preview Images
	c.OnXML(`//*[@id="content"]/div/div[2]/div[1]/div[2]/div[3]/div/div//a[@data-fancybox="gallery"]`, func(e *colly.XMLElement) {
		if href := e.Attr("href"); href != "" {
			info.PreviewImages = append(info.PreviewImages, e.Request.AbsoluteURL(href))
		}
	})

	// Fields
	c.OnXML(`/html/head/script[@type="application/ld+json"]`, func(e *colly.XMLElement) {
		data := struct {
			Type string `json:"@type"`
			// `Movie`
			Name          string   `json:"name"`
			Description   string   `json:"description"`
			Image         string   `json:"image"`
			Identifier    []string `json:"identifier"`
			DatePublished string   `json:"datePublished"`
			Duration      string   `json:"duration"`
			Actor         []string `json:"actor"`
			Genre         []string `json:"genre"`
			Director      string   `json:"director"`
			// `CreativeWorkSeries`
			AggregateRating struct {
				BestRating  float64 `json:"bestRating"`
				WorstRating float64 `json:"worstRating"`
				RatingCount int     `json:"ratingCount"`
				RatingValue float64 `json:"ratingValue"`
			}
			// `WebPage`
			URL string `json:"url"`
		}{}
		if json.Unmarshal([]byte(e.Text), &data) == nil {
			switch data.Type {
			case "Movie":
				if data.Name != "" {
					info.Title = data.Name
				}
				if info.Summary == "" {
					info.Summary = data.Description
				}
				if data.Director != "" {
					// Use director as maker.
					info.Maker = data.Director
				}
				if len(info.Genres) == 0 {
					info.Genres = removeEmpty(data.Genre)
				}
				if len(data.Actor) > 0 {
					info.Actors = removeEmpty(data.Actor)
				}
				for _, identifier := range data.Identifier {
					if num := fc2util.ParseNumber(identifier); num != "" {
						info.Number = fmt.Sprintf("FC2-%s", num)
						break
					}
				}
				info.CoverURL = data.Image
				info.ReleaseDate = parser.ParseDate(data.DatePublished)
				info.Runtime = parser.ParseRuntime(data.Duration)
			case "CreativeWorkSeries":
				// Average rating score.
				info.Score = data.AggregateRating.RatingValue
			case "WebPage":
				//if data.URL != "" {
				//	// Update homepage URL.
				//	info.Homepage = data.URL
				//}
			}
		}
	})

	// Cover (fallback)
	c.OnScraped(func(_ *colly.Response) {
		if info.CoverURL == "" && len(info.PreviewImages) > 0 {
			info.CoverURL = info.PreviewImages[0]
		}
		// cover as thumb image.
		info.ThumbURL = info.CoverURL
	})

	// Homepage (update)
	c.OnScraped(func(_ *colly.Response) {
		if info.ID != "" && len(info.Number) > 4 && info.Title != "" {
			info.Homepage = fmt.Sprintf(movieURL,
				strings.SplitN(info.ID, "-", 2)[0],
				info.Number[4:],
				url.PathEscape(info.Title))
		}
	})

	err = c.Visit(info.Homepage)
	return
}

func (fc2hub *FC2HUB) normalizeMovieURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Host == fc2hub.URL().Host {
		u.Scheme = fc2hub.URL().Scheme
	}
	return u.String(), nil
}

func (fc2hub *FC2HUB) NormalizeMovieKeyword(keyword string) string {
	return fc2util.ParseNumber(keyword)
}

func (fc2hub *FC2HUB) SearchMovie(keyword string) (results []*model.MovieSearchResult, err error) {
	targetURL := fmt.Sprintf(searchURL, url.QueryEscape(keyword))

	fc2hub.scrapeMu.Lock()
	defer fc2hub.scrapeMu.Unlock()

	c := fc2hub.ClonedCollector()
	session := fc2hub.bootstrapCookies(targetURL)
	fc2hub.applyCookieSession(c, session)
	c.ParseHTTPErrorResponse = true
	c.SetRedirectHandler(func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	})

	var detailURL string
	var responseErr error
	c.OnResponse(func(r *colly.Response) {
		loc, parseErr := url.Parse(r.Request.AbsoluteURL(r.Headers.Get("Location")))
		if parseErr != nil {
			responseErr = parseErr
			return
		}
		if regexp.MustCompile(`/video/\d+/id\d+`).MatchString(loc.Path) {
			detailURL = loc.String()
		}
	})

	if err = c.Visit(targetURL); err != nil {
		return nil, err
	}
	if responseErr != nil {
		return nil, responseErr
	}
	if detailURL == "" {
		return
	}

	detailURL, err = fc2hub.normalizeMovieURL(detailURL)
	if err != nil {
		return nil, err
	}
	detailCollector := fc2hub.ClonedCollector()
	fc2hub.applyCookieSession(detailCollector, session)
	var info *model.MovieInfo
	if info, err = fc2hub.getMovieInfoByURL(detailURL, detailCollector); err != nil {
		return nil, err
	}
	results = append(results, info.ToSearchResult())
	return
}

func (fc2hub *FC2HUB) bootstrapCookies(targetURL string) *cfCookieSession {
	endpoint, err := url.Parse(fc2hub.cookieURL)
	if err != nil {
		return fc2hub.currentCookieSession()
	}
	query := endpoint.Query()
	query.Set("url", targetURL)
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequest(http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fc2hub.currentCookieSession()
	}
	resp, err := fc2hub.cookieClient.Do(req)
	if err != nil {
		return fc2hub.currentCookieSession()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fc2hub.currentCookieSession()
	}

	var data cfCookieResponse
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || json.Unmarshal(body, &data) != nil ||
		len(data.Cookies) == 0 || strings.TrimSpace(data.UserAgent) == "" {
		return fc2hub.currentCookieSession()
	}

	session := &cfCookieSession{
		cookies:   make(map[string]string, len(data.Cookies)),
		userAgent: data.UserAgent,
		expiresAt: fc2hub.now().Add(cookieSessionTTL),
	}
	for name, value := range data.Cookies {
		if name == "" {
			return fc2hub.currentCookieSession()
		}
		session.cookies[name] = value
	}
	fc2hub.cookieSession = session
	return session
}

func (fc2hub *FC2HUB) currentCookieSession() *cfCookieSession {
	if fc2hub.cookieSession == nil || !fc2hub.now().Before(fc2hub.cookieSession.expiresAt) {
		fc2hub.cookieSession = nil
		return nil
	}
	return fc2hub.cookieSession
}

func (fc2hub *FC2HUB) applyCookieSession(c *colly.Collector, session *cfCookieSession) {
	nextNames := make(map[string]struct{})
	if session != nil {
		c.UserAgent = session.userAgent
		cookies := make([]*http.Cookie, 0, len(session.cookies))
		for name, value := range session.cookies {
			nextNames[name] = struct{}{}
			cookies = append(cookies, &http.Cookie{Name: name, Value: value, Path: "/"})
		}
		_ = c.SetCookies(cookieOrigin, cookies)
	}

	for name := range fc2hub.managedCookieNames {
		if _, keep := nextNames[name]; !keep {
			_ = c.SetCookies(cookieOrigin, []*http.Cookie{{Name: name, Path: "/", MaxAge: -1}})
		}
	}
	fc2hub.managedCookieNames = nextNames
}

func (fc2hub *FC2HUB) clearManagedCookies() {
	if len(fc2hub.managedCookieNames) == 0 {
		return
	}
	c := fc2hub.ClonedCollector()
	for name := range fc2hub.managedCookieNames {
		_ = c.SetCookies(cookieOrigin, []*http.Cookie{{Name: name, Path: "/", MaxAge: -1}})
	}
	fc2hub.managedCookieNames = make(map[string]struct{})
}

func removeEmpty(in []string) (out []string) {
	if len(in) == 0 {
		return in
	}
	out = make([]string, 0, len(in))
	for _, elem := range in {
		if strings.TrimSpace(elem) != "" {
			out = append(out, elem)
		}
	}
	return
}

func init() {
	provider.Register(Name, New)
}
