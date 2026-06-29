// feishin.go — endpoint spec for the Feishin client (pure data, no behaviour).
package main

var feishinSpec = clientSpec{
	Name: "feishin",
	Endpoints: []endpoint{
		{
			// Cover art image.
			Paths: []string{
				"/rest/getCoverArt",
				"/rest/getCoverArt.view",
			},
			Method:       "GET",
			Kind:         kindImage,
			UpstreamPath: "/rest/getCoverArt.view",
		},
		{
			// Album metadata (POST); response URLs are rewritten to the proxy.
			Paths: []string{
				"/rest/getAlbumInfo",
				"/rest/getAlbumInfo.view",
				"/rest/getAlbumInfo2",
				"/rest/getAlbumInfo2.view",
			},
			Method:       "POST",
			Kind:         kindMetadata,
			UpstreamPath: "/rest/getAlbumInfo2.view",
		},
		{
			// Share image (token in path); target of the rewritten metadata URLs.
			// UpstreamPath is unused for kindShareImage (path is forwarded as-is).
			PathPrefix: "/share/img/",
			Method:     "GET",
			Kind:       kindShareImage,
		},
	},
}
