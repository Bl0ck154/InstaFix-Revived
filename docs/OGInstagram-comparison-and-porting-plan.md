# OGInstagram comparison and porting plan

This note is for future AI agents working on `InstaFix-Revived`.

Compared projects:

- Our public project: `https://github.com/Bl0ck154/InstaFix-Revived`
- Reference project: `https://github.com/LilasKR/OGInstagram`

## Summary

`OGInstagram` does **not** implement cookie authorization. It avoids some anonymous Instagram blocking by using residential proxy sessions and a mobile-app-like GraphQL request. Our `auth-helper` is much more complete for cookie-based fallback, but accounts can still get checkpointed/banned, so the useful parts to borrow are mostly:

- additional public GraphQL strategy;
- broader parser support for mobile/V1 response shapes;
- better carousel/video URL extraction;
- direct/offload media routing ideas;
- oversized video protection;
- optional proxy/racing ideas for the future.

## Cookie/auth comparison

### InstaFix-Revived

Our auth-helper already has:

- `curl_cffi` browser-like TLS impersonation;
- cookie files and cookie directory pool;
- account id detection via `ds_user_id`;
- round-robin cookie selection;
- per-account cooldown for `login_required`, `checkpoint_required`, `challenge_required`, `cookie_missing`;
- global auth circuit breaker;
- authenticated request rate limits;
- positive and negative auth cache;
- `/healthz` and `/accounts`;
- authenticated oEmbed fallback;
- authenticated `api/v1/media/{media_id}/info/` fallback;
- optional video URL lookup and video proxy with range/resume/refresh.

### OGInstagram

No cookie pool and no cookie auth. Instead it uses:

- Decodo residential proxy credentials;
- 10 US proxy sessions;
- per-session hourly limit;
- session rotation on 401/403/429/5xx/connection failures;
- fetch racing: 2 initial sessions plus 1 hedged session after 1.5s;
- mobile app User-Agent and GraphQL `doc_id=8845758582119845`.

Its README says private, age-restricted, and US-unavailable posts are not supported.

## Useful OGInstagram details to borrow

### Alternative GraphQL strategy

`OGInstagram` posts to:

```text
https://www.instagram.com/graphql/query/
```

with:

```text
doc_id=8845758582119845
variables={"shortcode":"..."}
server_timestamps=true
User-Agent=Instagram 273.0.0.16.70 (iPhone15,2; iOS 17_5_1; en_US; en-US; scale=3.00; 1290x2796; 470085518)
```

This should be used as a second public GraphQL strategy after our existing web/Polaris query.

### Parser response shapes

`OGInstagram` parses these media roots:

- `data.xdt_shortcode_media`
- `data.shortcode_media`
- `data.xdt_api__v1__media__shortcode__web_info.items.0`

The V1/mobile shape is especially useful for difficult posts.

### Better media extraction

Useful paths:

- XDT sidecar: `edge_sidecar_to_children.edges[].node`
- V1 carousel: `carousel_media[]`
- Images:
  - `display_url`
  - best of `display_resources[].src`
  - best of `thumbnail_resources[].src`
  - best of `image_versions2.candidates[].url`
  - `thumbnail_url`
  - `thumbnail_src`
- Videos:
  - `video_url`
  - best of `video_versions[].url`
  - best of `video_resources[].url/src`
- Video detection:
  - `is_video == true`
  - `media_type == 2`
  - `__typename == XDTGraphVideo` / `GraphVideo`
  - typename contains `video`
- Dimensions:
  - `dimensions.width/height`
  - `original_width/original_height`
  - `width/height`
  - candidate width/height/config_width/config_height.

### Rendering/routing ideas

- Use a stable `/offload/{shortcode}/{index}` route that redirects to current media URL, with `?thumbnail=1` for thumbnails.
- Avoid exposing raw Instagram CDN URLs directly in embed HTML where possible.
- Detect oversized video with HEAD and avoid inline video cards above a configured threshold.
- For Discord/ActivityPub, OGInstagram exposes up to 3 images from a carousel. This is a future enhancement, not required for the initial port.

## Initial implementation checklist

- [x] Save this analysis for future agents.
- [x] Add parser helpers inspired by OGInstagram.
- [x] Parse `xdt_api__v1__media__shortcode__web_info.items.0`.
- [x] Support `carousel_media`.
- [x] Support `image_versions2.candidates`, `display_resources`, `thumbnail_resources`.
- [x] Support `video_versions` and `video_resources`.
- [x] Add alternative mobile GraphQL strategy with `doc_id=8845758582119845`.
- [x] Add offload media route.
- [x] Add optional oversized inline video protection.
- [x] Add optional public GraphQL proxy pool, disabled by default.
- [x] Add hedged/racing public fetches when multiple client attempts are available.
- [x] Add structured GraphQL error reasons.
- [x] Add runtime doc_id override for web GraphQL.
- [x] Add additional carousel image tags for clients that support multi-image previews.

## Future work still worth exploring

- More client-specific carousel rendering if a target preview client needs custom markup.
