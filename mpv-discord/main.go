package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/tnychn/mpv-discord/discordrpc"
	"github.com/tnychn/mpv-discord/mpvrpc"
)

var (
	client        *mpvrpc.Client
	presence      *discordrpc.Presence
	discordToken  string
	tinyToken     string
)

// urlCache maps thumbnail source (URL or local path) -> final TinyURL,
// so we don't re-upload the same image on every 1-second poll tick.
var urlCache = map[string]string{}

// cachedAppId is fetched once on first upload and reused for all subsequent ones.
var cachedAppId = ""

func init() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Lmsgprefix)

	client = mpvrpc.NewClient()
	presence = discordrpc.NewPresence(os.Args[2])
	discordToken = os.Args[3]
	tinyToken = os.Args[4]
}

var currTime int64 = time.Now().Local().UnixMilli()

func refreshCurrTime() {
	currTime = time.Now().Local().UnixMilli()
}

// getAppId fetches the application ID from Discord using the bot token.
func getAppId() (string, error) {
	if discordToken == "" {
		return "", errors.New("discord token not provided")
	}
	req, _ := http.NewRequest("GET", "https://discord.com/api/v10/applications/@me", nil)
	req.Header.Set("Authorization", "Bot "+discordToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.ID, nil
}

// uploadToDiscord uploads a local image file path OR fetches a remote URL
// and uploads its bytes to Discord as an ephemeral application attachment.
// Returns the raw Discord CDN URL (with expiring query params).
func uploadToDiscord(appId string, imageSource string) (string, error) {
	var imageBytes []byte
	var err error

	if strings.HasPrefix(imageSource, "https://") {
		// Remote URL (e.g. YouTube thumbnail) — download first
		resp, err := http.Get(imageSource)
		if err != nil {
			return "", fmt.Errorf("failed to download image: %w", err)
		}
		defer resp.Body.Close()
		imageBytes, err = io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("failed to read image body: %w", err)
		}
	} else {
		// Local file path (e.g. ffmpeg-extracted frame)
		imageBytes, err = os.ReadFile(imageSource)
		if err != nil {
			return "", fmt.Errorf("failed to read local image: %w", err)
		}
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "thumbnail.jpg")
	part.Write(imageBytes)
	writer.Close()

	url := fmt.Sprintf("https://discord.com/api/v10/applications/%s/attachment", appId)
	req, _ := http.NewRequest("POST", url, body)
	req.Header.Set("Authorization", "Bot "+discordToken)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Attachment struct {
			URL string `json:"url"`
		} `json:"attachment"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Attachment.URL, nil
}

// toTinyURL shortens a URL using TinyURL API so Discord can't strip the query params.
func toTinyURL(rawURL string) (string, error) {
	if tinyToken == "" {
		return "", errors.New("tinyurl token not provided")
	}
	payload, _ := json.Marshal(map[string]string{"url": rawURL})
	req, _ := http.NewRequest("POST", "https://api.tinyurl.com/create?api_token="+tinyToken, bytes.NewBuffer(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		Data struct {
			TinyURL string `json:"tiny_url"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Data.TinyURL, nil
}

// getThumbnailImageKey reads the thumbnail source set by discord.lua
// (either a YouTube https:// URL or a local file path set via user-data),
// uploads it to Discord, shortens via TinyURL, caches and returns the result.
// Falls back to "mpv" asset key on any error or if tokens aren't configured.
func getThumbnailImageKey() string {
	if discordToken == "" || tinyToken == "" {
		return "mpv"
	}

	source, err := client.GetPropertyString("user-data/discord-thumbnail")
	if err != nil || source == "" || source == "mpv" {
		return "mpv"
	}

	// Return cached TinyURL if we've already processed this source
	if cached, ok := urlCache[source]; ok {
		return cached
	}

	if cachedAppId == "" {
		cachedAppId, err = getAppId()
		if err != nil {
			log.Printf("(thumbnail): failed to get app ID: %v", err)
			return "mpv"
		}
	}

	cdnURL, err := uploadToDiscord(cachedAppId, source)
	if err != nil {
		log.Printf("(thumbnail): Discord upload failed: %v", err)
		return "mpv"
	}

	tinyURL, err := toTinyURL(cdnURL)
	if err != nil {
		log.Printf("(thumbnail): TinyURL failed: %v", err)
		return "mpv"
	}

	log.Printf("(thumbnail): %s -> %s", source, tinyURL)
	urlCache[source] = tinyURL
	return tinyURL
}

func getActivity() (activity discordrpc.Activity, err error) {
	getProperty := func(key string) (prop interface{}) {
		prop, err = client.GetProperty(key)
		return
	}
	getPropertyString := func(key string) (prop string) {
		prop, err = client.GetPropertyString(key)
		return
	}

	// Large Image
	activity.LargeImageKey = getThumbnailImageKey()
	activity.LargeImageText = "mpv"
	if version := getPropertyString("mpv-version"); version != "" {
		activity.LargeImageText += " " + version[4:]
	}

	// Details
	activity.Details = getPropertyString("media-title")
	fileFormat := getPropertyString("file-format")
	metaTitle := getProperty("metadata/by-key/Title")
	metaArtist := getProperty("metadata/by-key/Artist")
	metaAlbum := getProperty("metadata/by-key/Album")
	if metaTitle != nil {
		activity.Details = metaTitle.(string)
	}

	// State
	if metaArtist != nil {
		activity.State += " by " + metaArtist.(string)
	}
	if metaAlbum != nil {
		activity.State += " on " + metaAlbum.(string)
	}
	if activity.State == "" {
		if aid, ok := getProperty("aid").(string); !ok || aid != "false" {
			activity.Type = 2
			activity.State += "Audio"
		}
		activity.State += "/"
		if vid, ok := getProperty("vid").(string); !ok || vid != "false" {
			activity.State += "Video"
			activity.Type = 3
		}
		activity.State += (": " + fileFormat)
	}

	// Small Image
	buffering := getProperty("paused-for-cache")
	pausing := getProperty("pause")
	loopingFile := getPropertyString("loop-file")
	loopingPlaylist := getPropertyString("loop-playlist")
	if buffering != nil && buffering.(bool) {
		activity.SmallImageKey = "buffer"
		activity.SmallImageText = "Buffering"
	} else if pausing != nil && pausing.(bool) {
		activity.SmallImageKey = "pause"
		activity.SmallImageText = "Paused"
	} else if loopingFile != "no" || loopingPlaylist != "no" {
		activity.SmallImageKey = "loop"
		activity.SmallImageText = "Looping"
	} else {
		activity.SmallImageKey = "play"
		activity.SmallImageText = "Playing"
	}
	if percentage := getProperty("percent-pos"); percentage != nil {
		activity.SmallImageText += fmt.Sprintf(" (%d%%)", int(percentage.(float64)))
	}
	if pcount := getProperty("playlist-count"); pcount != nil && int(pcount.(float64)) > 1 {
		if ppos := getProperty("playlist-pos-1"); ppos != nil {
			activity.SmallImageText += fmt.Sprintf(" [%d/%d]", int(ppos.(float64)), int(pcount.(float64)))
		}
	}

	// Timestamps
	_duration := getProperty("duration")
	durationMillis := int64(_duration.(float64))
	_timePos := getProperty("time-pos")
	timePosMills := int64(_timePos.(float64))

	startTimePos := currTime - (timePosMills * 1000)
	duration := startTimePos + (durationMillis * 1000)

	if pausing != nil && !pausing.(bool) {
		activity.Timestamps = &discordrpc.ActivityTimestamps{
			Start: startTimePos,
			End:   duration,
		}
		refreshCurrTime()
	}
	return
}

func openClient() {
	if err := client.Open(os.Args[1]); err != nil {
		log.Fatalln(err)
	}
	log.Println("(mpv-ipc): connected")
}

func openPresence() {
	for range time.Tick(500 * time.Millisecond) {
		if client.IsClosed() {
			return
		}
		if err := presence.Open(); err == nil {
			break
		}
	}
	log.Println("(discord-ipc): connected")
}

func main() {
	defer func() {
		if !client.IsClosed() {
			if err := client.Close(); err != nil {
				log.Fatalln(err)
			}
			log.Println("(mpv-ipc): disconnected")
		}
		if !presence.IsClosed() {
			if err := presence.Close(); err != nil {
				log.Fatalln(err)
			}
			log.Println("(discord-ipc): disconnected")
		}
	}()

	openClient()
	go openPresence()

	for range time.Tick(time.Second) {
		activity, err := getActivity()
		if err != nil {
			if errors.Is(err, syscall.EPIPE) {
				break
			} else if !errors.Is(err, io.EOF) {
				log.Println(err)
				continue
			}
		}
		if !presence.IsClosed() {
			go func() {
				if err = presence.Update(activity); err != nil {
					if errors.Is(err, syscall.EPIPE) {
						if err = presence.Close(); err != nil {
							log.Fatalln(err)
						}
						log.Println("(discord-ipc): reconnecting...")
						go openPresence()
					} else if !errors.Is(err, io.EOF) {
						log.Println(err)
					}
				}
			}()
		}
	}
}
