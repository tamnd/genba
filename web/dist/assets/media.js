// Audio and video.
//
// The transcript is the document. It is what was indexed, it is what a query
// matched, and it is the half somebody can read at the speed they read rather
// than at the speed somebody spoke. So the transcript is on the page from the
// first paint, and the player is offered underneath it.
//
// The player loads on a press rather than on arrival. Every request in this
// interface carries the caller's identity in a header, which a src attribute
// cannot do, so the bytes come through fetch and become an object URL, and that
// means the whole file. Starting that automatically would spend two hundred
// megabytes to show somebody a page they opened to read one sentence of.

import { h, replace } from "./dom.js";
import { api } from "./api.js";
import { bytes as formatBytes } from "./format.js";

/**
 * player is the control that fetches a recording and plays it.
 *
 * A document whose bytes the server will not hand over is not an error worth a
 * dialog. The transcript above it is still the document, so the failure is one
 * line under the button that offered to play something.
 */
export function player(d) {
  const kind = (d.media_type || "").startsWith("video/") ? "video" : "audio";
  const size = Number((d.properties && d.properties.size_bytes) || 0);
  const slot = h("div", { class: "media__slot" });

  const load = async (button) => {
    button.disabled = true;
    button.textContent = "Loading";
    try {
      const res = await api.content(d.id);
      const el = h(kind, {
        class: `media__player media__player--${kind}`,
        src: URL.createObjectURL(res.blob),
        controls: true,
        preload: "metadata",
      });
      // Nothing here is autoplay. The press was a request to load it, and a
      // recording that starts talking on its own is the behaviour every browser
      // spent a decade learning to block.
      replace(slot, el);
      el.focus();
    } catch {
      button.disabled = false;
      button.textContent = kind === "video" ? "Play the video" : "Play the recording";
      replace(slot, h("p", { class: "media__failed" }, "This recording is not available to play."));
    }
  };

  return h(
    "div",
    { class: "media" },
    h(
      "div",
      { class: "media__bar" },
      h(
        "button",
        {
          class: "button button--primary",
          type: "button",
          onClick: (e) => load(e.currentTarget),
        },
        kind === "video" ? "Play the video" : "Play the recording",
      ),
      size > 0 && h("span", { class: "media__size" }, formatBytes(size)),
    ),
    slot,
  );
}
