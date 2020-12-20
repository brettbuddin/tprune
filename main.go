package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dghubble/go-twitter/twitter"
	"github.com/dghubble/oauth1"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func main() {
	var (
		cfg          config
		keepIDs      string
		keepKeywords string
	)
	flagset := flag.NewFlagSet("tprune", flag.ExitOnError)
	flagset.StringVar(&cfg.username, "username", "", "Username to target")
	flagset.StringVar(&cfg.consumerKey, "consumer-key", "", "Twitter Consumer Key")
	flagset.StringVar(&cfg.consumerSecret, "consumer-secret", "", "Twitter Consumer Secret")
	flagset.StringVar(&cfg.oauthToken, "oauth-token", "", "Twitter OAuth Token")
	flagset.StringVar(&cfg.oauthTokenSecret, "oauth-token-secret", "", "Twitter OAuth Token Secret")
	flagset.DurationVar(&cfg.retention.maxAge, "max-age", 0, "Maximum age to keep. Tweets older than this will be deleted.")
	flagset.StringVar(&cfg.logLevel, "log-level", "info", "Log level")
	flagset.StringVar(&keepIDs, "keep-ids", "", "Tweet IDs to keep forever.")
	flagset.StringVar(&keepKeywords, "keep-keywords", "", "Tweet keywords to keep forever.")
	if err := flagset.Parse(os.Args[1:]); err != nil {
		fmt.Println(err)
		os.Exit(2)
	}

	// Build and validate configuration
	int64KeepIDs, err := parseKeepIDs(keepIDs)
	if err != nil {
		fmt.Println(err)
		flagset.Usage()
		os.Exit(2)
	}
	cfg.retention.ids = int64KeepIDs
	cfg.retention.keywords = parseKeepKeywords(keepKeywords)
	if err := cfg.validate(); err != nil {
		fmt.Println(err)
		flagset.Usage()
		os.Exit(2)
	}

	// Do it to it, Lars!
	if err := run(cfg); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

type config struct {
	username                     string
	consumerKey, consumerSecret  string
	oauthToken, oauthTokenSecret string
	retention                    retention
	logLevel                     string
}

func (cfg config) validate() error {
	if cfg.username == "" {
		return fmt.Errorf("-username is required")
	}
	if cfg.consumerKey == "" {
		return fmt.Errorf("-consumer-key is required")
	}
	if cfg.consumerSecret == "" {
		return fmt.Errorf("-consumer-secret is required")
	}
	if cfg.oauthToken == "" {
		return fmt.Errorf("-oauth-token is required")
	}
	if cfg.oauthTokenSecret == "" {
		return fmt.Errorf("-oauth-token-secret is required")
	}
	if cfg.retention.maxAge == 0 {
		return fmt.Errorf("-max-age is required")
	}
	return nil
}

func run(cfg config) error {
	logger, err := newLogger(cfg.logLevel)
	if err != nil {
		return fmt.Errorf("failed to setup logger: %w", err)
	}
	defer logger.Sync()

	var (
		config     = oauth1.NewConfig(cfg.consumerKey, cfg.consumerSecret)
		token      = oauth1.NewToken(cfg.oauthToken, cfg.oauthTokenSecret)
		httpClient = config.Client(context.Background(), token)
		client     = twitter.NewClient(httpClient)
	)

	account, _, err := client.Accounts.VerifyCredentials(nil)
	if err != nil {
		return fmt.Errorf("failed to verify credentials: %w", err)
	}
	logger.Info("Verified credentials",
		zap.String("id", account.IDStr),
		zap.String("username", account.ScreenName))

	tweetFetcher := newTweetFetcher(client, account.ScreenName)
	favoriteFetcher := newFavoriteFetcher(client, account.ID)
	destroyer := newDestroyer(client, cfg.retention)

	for tweetFetcher.fetch() {
		if tweetFetcher.err != nil {
			return fmt.Errorf("failed to fetch: %w", tweetFetcher.err)
		}
		for _, t := range tweetFetcher.tweets {
			if err := destroyer.destroyTweet(logger, t); err != nil {
				return fmt.Errorf("failed to delete: %w", err)
			}
		}
	}

	for favoriteFetcher.fetch() {
		if favoriteFetcher.err != nil {
			return fmt.Errorf("failed to fetch: %w", favoriteFetcher.err)
		}
		for _, t := range favoriteFetcher.tweets {
			if err := destroyer.destroyFavorite(logger, t); err != nil {
				return fmt.Errorf("failed to delete: %w", err)
			}
		}
	}

	return nil
}

// tweetFetcher steps across all tweets in a username's timeline
type tweetFetcher struct {
	client   *twitter.Client
	username string
	maxID    int64

	tweets []twitter.Tweet
	err    error
}

// newTweetFetcher returns a new fetcher
func newTweetFetcher(client *twitter.Client, username string) *tweetFetcher {
	return &tweetFetcher{
		client:   client,
		username: username,
	}
}

// fetch gets a list of tweets. It should be called continuously as an iterator.
// A return value of "true" means there are potentially more tweets to be
// fetched. A value of "false" means there are no more tweets to be fetched.
//
// The resulting tweets are stored in the "tweets" struct field. Any errors that
// occur will be reflected in the "err" field.
func (f *tweetFetcher) fetch() bool {
	var (
		resp   *http.Response
		err    error
		on     = true
		params = &twitter.UserTimelineParams{
			ScreenName:      f.username,
			Count:           200,
			MaxID:           f.maxID,
			IncludeRetweets: &on,
			TrimUser:        &on,
		}
	)
	f.tweets, resp, err = f.client.Timelines.UserTimeline(params)
	if err != nil {
		if resp.StatusCode == http.StatusTooManyRequests {
			if err := backOff(resp.Header); err != nil {
				f.err = fmt.Errorf("failed to back off: %w", err)
				return false
			}
		} else {
			f.err = fmt.Errorf("failed to fetch tweets: %w", err)
			return false
		}
	}
	if len(f.tweets) > 0 {
		f.maxID = f.tweets[len(f.tweets)-1].ID - 1
		return true
	}
	return false
}

// favoriteFetcher fetches favorited tweets
type favoriteFetcher struct {
	client    *twitter.Client
	accountID int64
	maxID     int64

	tweets []twitter.Tweet
	err    error
}

// newFavoriteFetcher returns a new favorite fetcher
func newFavoriteFetcher(client *twitter.Client, accountID int64) *favoriteFetcher {
	return &favoriteFetcher{
		client:    client,
		accountID: accountID,
	}
}

// fetch gets a list of favorited tweets. It should be called continuously as an
// iterator. A return value of "true" means there are potentially more tweets to
// be fetched. A value of "false" means there are no more tweets to be fetched.
//
// The resulting tweets are stored in the "tweets" struct field. Any errors that
// occur will be reflected in the "err" field.
func (f *favoriteFetcher) fetch() bool {
	var (
		resp   *http.Response
		err    error
		params = &twitter.FavoriteListParams{
			UserID: f.accountID,
			Count:  200,
			MaxID:  f.maxID,
		}
	)
	f.tweets, resp, err = f.client.Favorites.List(params)
	if err != nil {
		if resp.StatusCode == http.StatusTooManyRequests {
			if err := backOff(resp.Header); err != nil {
				f.err = fmt.Errorf("failed to back off: %w", err)
				return false
			}
		} else {
			f.err = fmt.Errorf("failed to fetch tweets: %w", err)
			return false
		}
	}
	if len(f.tweets) > 0 {
		f.maxID = f.tweets[len(f.tweets)-1].ID - 1
		return true
	}
	return false
}

// destroyer deletes tweets and favorites based on retention rules
type destroyer struct {
	client    *twitter.Client
	now       time.Time
	retention retention
}

// newDestroyer returns a new destroyer
func newDestroyer(client *twitter.Client, r retention) destroyer {
	return destroyer{
		client:    client,
		now:       time.Now(),
		retention: r,
	}
}

// destroyTweet deletes a tweet
func (d destroyer) destroyTweet(logger *zap.Logger, t twitter.Tweet) error {
	logger = logger.With(
		zap.Int64("id", t.ID))

	evict, err := d.retention.isTombstoned(logger, t, d.now)
	if err != nil {
		return err
	}
	if !evict {
		logger.Info("Keeping Tweet")
		return nil
	}

	logger.Info("Deleting Tweet")
	_, resp, err := d.client.Statuses.Destroy(t.ID, nil)
	if err != nil {
		if resp.StatusCode == http.StatusTooManyRequests {
			if err := backOff(resp.Header); err != nil {
				return fmt.Errorf("failed to back off: %w", err)
			}
		} else {
			return err
		}
	}
	return nil
}

// destroyFavorite deletes a favorited tweet
func (d destroyer) destroyFavorite(logger *zap.Logger, t twitter.Tweet) error {
	logger = logger.With(
		zap.Int64("id", t.ID))

	evict, err := d.retention.isTombstoned(logger, t, d.now)
	if err != nil {
		return err
	}
	if !evict {
		logger.Info("Keeping Favorite")
		return nil
	}

	logger.Info("Deleting Favorite")
	_, resp, err := d.client.Favorites.Destroy(&twitter.FavoriteDestroyParams{
		ID: t.ID,
	})
	if err != nil {
		if resp.StatusCode == http.StatusTooManyRequests {
			if err := backOff(resp.Header); err != nil {
				return fmt.Errorf("failed to back off: %w", err)
			}
		} else {
			return err
		}
	}
	return nil
}

// backOff extracts rate-limit back-off information from the response and sleeps
// that number of seconds.
func backOff(header http.Header) error {
	reset, err := strconv.Atoi(header.Get("X-Rate-Limit-Reset"))
	if err != nil {
		return err
	}
	time.Sleep(time.Duration(reset) * time.Second)
	return nil
}

// retention is the retention policy
type retention struct {
	ids      []int64
	keywords []string
	maxAge   time.Duration
}

// isTombstoned determines whether or not a tweet should be deleted
func (r retention) isTombstoned(logger *zap.Logger, t twitter.Tweet, now time.Time) (bool, error) {
	createdAt, err := t.CreatedAtTime()
	if err != nil {
		return false, err
	}
	age := now.Sub(createdAt)

	if age < r.maxAge {
		return false, nil
	}
	for _, id := range r.ids {
		if id == t.ID {
			return false, nil
		}
	}
	for _, keyword := range r.keywords {
		if strings.Contains(t.Text, keyword) {
			return false, nil
		}
	}
	return true, nil
}

func parseKeepIDs(v string) ([]int64, error) {
	if len(v) == 0 {
		return nil, nil
	}
	var int64s []int64
	for _, s := range strings.Split(v, ",") {
		p, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, err
		}
		int64s = append(int64s, p)
	}
	return int64s, nil
}

func parseKeepKeywords(v string) []string {
	if len(v) == 0 {
		return nil
	}
	return strings.Split(v, ",")
}

func newLogger(logLevel string) (*zap.Logger, error) {
	var lvl zapcore.Level
	err := lvl.Set(logLevel)
	if err != nil {
		return nil, fmt.Errorf("setting log level to %s: %v", logLevel, err)
	}
	return zap.NewDevelopment(zap.IncreaseLevel(lvl))
}
