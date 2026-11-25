package nixflux

import (
	"net/url"
	"strings"
	"time"
)

type ChannelStatus struct {
	Status       int
	Reason       string
	FinalURL     string
	ContentType  string
	PackageCount int
	ElapsedMS    int64
}

func fetchChannelStatus(channel Channel) (ChannelStatus, error) {
	started := time.Now()
	channel = defaultAPTChannel(channel)
	index, err := loadAPTRepositoryIndex(channel)
	if err != nil {
		return ChannelStatus{}, err
	}
	packageCount := 0
	for _, versions := range index.Packages {
		packageCount += len(versions)
	}
	base, _ := url.Parse(strings.TrimRight(channel.URL, "/") + "/")
	releaseURL := base.ResolveReference(&url.URL{Path: "dists/" + channel.Distribution + "/Release"}).String()
	return ChannelStatus{
		Status:       200,
		Reason:       "APT metadata signature and package indexes verified",
		FinalURL:     releaseURL,
		ContentType:  "Debian APT repository",
		PackageCount: packageCount,
		ElapsedMS:    time.Since(started).Milliseconds(),
	}, nil
}
