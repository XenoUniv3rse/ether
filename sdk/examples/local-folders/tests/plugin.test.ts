import { test } from "node:test";
import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { createMockHostClient } from "../../../typescript/src/index.ts";
import { createPlugin } from "../plugin.ts";
const folders = JSON.parse(
  readFileSync(new URL("./fixtures/folders.json", import.meta.url), "utf8"),
);
test("scan indexes movies and episodes, manual match survives a new instance, and stale files disappear", async () => {
  const host = createMockHostClient({
    folderRoots: ["/media", "/shows"],
    folders: new Map(Object.entries(folders)),
    catalog: new Map([
      [
        "a",
        {
          id: 1,
          title: "Example Movie",
          type: "movie",
          year: 2001,
          tmdbId: 10,
        },
      ],
      ["b", { id: 2, title: "Example Show", type: "show", tmdbId: 20 }],
    ]),
  });
  let p = createPlugin(host);
  await p.dispatch({ method: "POST", path: "/ether/job/scan" });
  let movie = await p.dispatch({ path: "/stream/movie/tmdb%3A10.json" });
  assert.equal(
    (movie.body as any).streams[0].ether.file,
    "/media/Example Movie (2001)/Example.Movie.2001.1080p.mkv",
  );
  let ep = await p.dispatch({ path: "/stream/series/tmdb%3A20%3A1%3A2.json" });
  assert.equal((ep.body as any).streams[0].ether.file, "/shows/Example Show/Season 1/Example.Show.S01E02.1080p.mkv");
  const page = await p.dispatch({ path: "/ether/view/matches" });
  assert.equal((page.body as any).data.folder0, "/media/Unmatched");
  await p.dispatch({
    method: "POST",
    path: "/ether/action/match",
    body: { root: "/media", folder: "Unmatched", tmdbId: 30, mediaType: "movie" },
  });
  p = createPlugin(host);
  assert.equal(
    ((await p.dispatch({ path: "/stream/movie/tmdb%3A30.json" })).body as any)
      .streams.length,
    1,
  );
  host.state.folderRoots = ["/shows"];
  await p.dispatch({ method: "POST", path: "/ether/job/scan" });
  assert.equal(
    ((await p.dispatch({ path: "/stream/movie/tmdb%3A10.json" })).body as any)
      .streams.length,
    0,
  );
});

test("folders match by provider ID tag, and titles match without punctuation", async () => {
  const dir = (name: string) => ({ name, path: name, dir: true });
  const file = (path: string) => ({
    name: path.split("/").pop(),
    path,
    dir: false,
  });
  const host = createMockHostClient({
    folderRoots: ["/media"],
    folders: new Map<string, unknown[]>([
      [
        "/media",
        [
          dir("The Matrix (1999) {tmdb-603}"),
          dir("Mr. Robot (2015) [imdbid-tt4158110]"),
          dir("Re ZERO (2016)"),
          dir("Wrong Kind (2000) {tmdb-5}"),
        ],
      ],
      [
        "/media/The Matrix (1999) {tmdb-603}",
        [file("The Matrix (1999) {tmdb-603}/The Matrix (1999).mkv")],
      ],
      [
        "/media/Mr. Robot (2015) [imdbid-tt4158110]",
        [file("Mr. Robot (2015) [imdbid-tt4158110]/Mr.Robot.S01E01.mkv")],
      ],
      ["/media/Re ZERO (2016)", [file("Re ZERO (2016)/Re.ZERO.S01E01.mkv")]],
      [
        "/media/Wrong Kind (2000) {tmdb-5}",
        [file("Wrong Kind (2000) {tmdb-5}/Wrong Kind (2000).mkv")],
      ],
    ]) as any,
    catalog: new Map([
      [
        "tmdb:603",
        { id: 1, title: "The Matrix", type: "movie", year: 1999, tmdbId: 603 },
      ],
      [
        "imdb:tt4158110",
        { id: 2, title: "Mr. Robot", type: "show", year: 2015, tmdbId: 62560 },
      ],
      ["tmdb:5", { id: 3, title: "Wrong Kind", type: "show", tmdbId: 5 }],
    ]),
  });
  // Ether's catalogue search is fuzzy; the folder title still has to equal
  // the catalogue title once punctuation is ignored.
  host.catalog.search = () => ({
    titles: [
      { id: 4, title: "Re:ZERO", type: "show", year: 2016, tmdbId: 65942 },
    ],
  });
  const p = createPlugin(host);
  await p.dispatch({ method: "POST", path: "/ether/job/scan" });
  const streams = async (path: string) =>
    ((await p.dispatch({ path })).body as any).streams.length;
  assert.equal(await streams("/stream/movie/tmdb%3A603.json"), 1);
  assert.equal(await streams("/stream/series/tmdb%3A62560%3A1%3A1.json"), 1);
  assert.equal(await streams("/stream/series/tmdb%3A65942%3A1%3A1.json"), 1);
  assert.equal(await streams("/stream/movie/tmdb%3A5.json"), 0);
  const page = await p.dispatch({ path: "/ether/view/matches" });
  assert.equal((page.body as any).data.folder0, "/media/Wrong Kind (2000) {tmdb-5}");
});
