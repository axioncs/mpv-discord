local msg = require("mp.msg")
local opts = require("mp.options")
local utils = require("mp.utils")

local options = {
	key = "D",
	active = true,
	client_id = "1328997690339758141",
	binary_path = "",
	socket_path = "/tmp/mpvsocket",
	use_static_socket_path = true,
	autohide_threshold = 0,
}
opts.read_options(options, "discord")

if options.binary_path == "" then
	msg.fatal("Missing binary path in config file.")
	os.exit(1)
end

function file_exists(path)
	local f = io.open(path, "r")
	if f ~= nil then
		io.close(f)
		return true
	else
		return false
	end
end

if not file_exists(options.binary_path) then
	msg.fatal("The specified binary path does not exist.")
	os.exit(1)
end

local version = "1.6.1"
msg.info(("mpv-discord v%s by CosmicPredator"):format(version))

local socket_path = options.socket_path
if not options.use_static_socket_path then
	local pid = utils.getpid()
	local filename = ("mpv-discord-%s"):format(pid)
	if socket_path == "" then
		socket_path = "/tmp/"
	end
	socket_path = utils.join_path(socket_path, filename)
elseif socket_path == "" then
	msg.fatal("Missing socket path in config file.")
	os.exit(1)
end
msg.info(("(mpv-ipc): %s"):format(socket_path))
mp.set_property("input-ipc-server", socket_path)

-- ============================================================
-- Thumbnail resolution
-- Sets user-data/discord-thumbnail to either:
--   - A YouTube thumbnail https:// URL  (Go will download + upload to Discord)
--   - A local file path to a ffmpeg frame  (Go will read + upload to Discord)
--   - "mpv" as fallback (Go shows the registered mpv asset)
-- ============================================================

local function get_youtube_id(url)
	if not url then return nil end
	return url:match("youtube%.com/watch%?.*v=([a-zA-Z0-9_%-]+)")
		or url:match("youtu%.be/([a-zA-Z0-9_%-]+)")
		or url:match("youtube%.com/shorts/([a-zA-Z0-9_%-]+)")
end

local function extract_local_frame(filepath)
	-- Extract a frame at 10s, fall back to first frame for short files
	-- The pad filter centers the frame in a square canvas (pillarbox/letterbox)
	-- so Discord doesn't crop the image. Commas in the filter must be escaped
	-- with \ to prevent ffmpeg treating them as filter separators.
	local tmpfile = os.tmpname() .. ".jpg"
	local pad = "'pad=max(iw\\,ih):max(iw\\,ih):(ow-iw)/2:(oh-ih)/2:black'"
	local cmd1 = string.format(
		"ffmpeg -y -ss 10 -i %q -vframes 1 -vf " .. pad .. " -q:v 5 %q -loglevel quiet 2>/dev/null",
		filepath, tmpfile
	)
	local cmd2 = string.format(
		"ffmpeg -y -i %q -vframes 1 -vf " .. pad .. " -q:v 5 %q -loglevel quiet 2>/dev/null",
		filepath, tmpfile
	)
	if os.execute(cmd1) ~= 0 then
		os.execute(cmd2)
	end
	-- Verify file exists and has content
	local f = io.open(tmpfile, "rb")
	if f then
		local size = f:seek("end")
		f:close()
		if size and size > 0 then
			return tmpfile
		end
		os.remove(tmpfile)
	end
	return nil
end

mp.register_event("file-loaded", function()
	local path = mp.get_property("path")
	if not path then return end

	-- Reset to default while resolving
	mp.set_property("user-data/discord-thumbnail", "mpv")

	local yt_id = get_youtube_id(path)
	if yt_id then
		-- Pass the YouTube thumbnail URL — Go will download and upload it
		local thumb_url = "https://img.youtube.com/vi/" .. yt_id .. "/hqdefault.jpg"
		mp.set_property("user-data/discord-thumbnail", thumb_url)
		msg.info("Discord thumbnail source (YouTube): " .. thumb_url)
	else
		-- Local file: extract a frame with ffmpeg, pass the temp file path to Go
		mp.add_timeout(0.5, function()
			msg.info("Discord: extracting thumbnail frame from local file...")
			local frame_path = extract_local_frame(path)
			if frame_path then
				mp.set_property("user-data/discord-thumbnail", frame_path)
				msg.info("Discord thumbnail source (local frame): " .. frame_path)
			else
				msg.warn("Discord: ffmpeg frame extraction failed, using default logo")
			end
		end)
	end
end)

-- ============================================================
-- End thumbnail resolution
-- ============================================================

local cmd = nil

local function start()
	if cmd == nil then
		cmd = mp.command_native_async({
			name = "subprocess",
			playback_only = false,
			args = {
				options.binary_path,
				socket_path,
				options.client_id,
			},
		}, function() end)
		msg.info("launched subprocess")
		mp.osd_message("Discord Rich Presence: Started")
	end
end

function stop()
	mp.abort_async_command(cmd)
	cmd = nil
	msg.info("aborted subprocess")
	mp.osd_message("Discord Rich Presence: Stopped")
end

if options.active then
	mp.register_event("file-loaded", start)
end

mp.add_key_binding(options.key, "toggle-discord", function()
	if cmd ~= nil then
		stop()
	else
		start()
	end
end)

mp.register_event("shutdown", function()
	if cmd ~= nil then
		stop()
	end
	if not options.use_static_socket_path then
		os.remove(socket_path)
	end
end)

if options.autohide_threshold > 0 then
	local timer = nil
	local t = options.autohide_threshold
	mp.observe_property("pause", "bool", function(_, value)
		if value == true then
			timer = mp.add_timeout(t, function()
				if cmd ~= nil then
					stop()
				end
			end)
		else
			if timer ~= nil then
				timer:kill()
				timer = nil
			end
			if options.active and cmd == nil then
				start()
			end
		end
	end)
end
