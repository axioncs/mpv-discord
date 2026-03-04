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

function file_exists(path) -- fix(#23): use this instead of utils.file_info
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
		socket_path = "/tmp/" -- default
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
-- ============================================================

local function get_youtube_id(url)
	if not url then return nil end
	return url:match("youtube%.com/watch%?.*v=([a-zA-Z0-9_%-]+)")
		or url:match("youtu%.be/([a-zA-Z0-9_%-]+)")
		or url:match("youtube%.com/shorts/([a-zA-Z0-9_%-]+)")
end

local function upload_thumbnail(filepath)
	local tmpfile = os.tmpname() .. ".jpg"

	-- Try extracting at 10s first, fall back to first frame for short files
	local cmd1 = string.format(
		'ffmpeg -y -ss 10 -i %q -vframes 1 -q:v 5 %q -loglevel quiet 2>/dev/null',
		filepath, tmpfile
	)
	local cmd2 = string.format(
		'ffmpeg -y -i %q -vframes 1 -q:v 5 %q -loglevel quiet 2>/dev/null',
		filepath, tmpfile
	)
	if os.execute(cmd1) ~= 0 then
		os.execute(cmd2)
	end

	-- Check the file actually got created
	local f = io.open(tmpfile, "r")
	if not f then
		msg.warn("Discord: ffmpeg failed to extract thumbnail frame")
		return nil
	end
	f:close()

	-- Upload to litterbox (expires in 1 hour)
	local curl_cmd = string.format(
		'curl -sf -F "reqtype=fileupload" -F "time=1h" -F "fileToUpload=@%s" '
		.. '"https://litterbox.catbox.moe/resources/internals/api.php"',
		tmpfile
	)
	local handle = io.popen(curl_cmd)
	if not handle then
		os.remove(tmpfile)
		msg.warn("Discord: curl failed to run")
		return nil
	end
	local url = handle:read("*a")
	handle:close()
	os.remove(tmpfile)

	url = url and url:match("^%s*(.-)%s*$") -- trim whitespace
	if url and url:match("^https://") then
		return url
	end
	msg.warn("Discord: upload returned unexpected response: " .. (url or "nil"))
	return nil
end

mp.register_event("file-loaded", function()
	local path = mp.get_property("path")
	if not path then return end

	-- Reset to default while resolving
	mp.set_property("user-data/discord-thumbnail", "mpv")

	local yt_id = get_youtube_id(path)
	if yt_id then
		local thumb = "https://img.youtube.com/vi/" .. yt_id .. "/hqdefault.jpg"
		mp.set_property("user-data/discord-thumbnail", thumb)
		msg.info("Discord thumbnail (YouTube): " .. thumb)
	else
		-- Run after a short delay so mpv startup isn't blocked
		mp.add_timeout(1.0, function()
			msg.info("Discord: extracting and uploading thumbnail for local file...")
			local url = upload_thumbnail(path)
			if url then
				mp.set_property("user-data/discord-thumbnail", url)
				msg.info("Discord thumbnail uploaded: " .. url)
			else
				msg.warn("Discord: thumbnail upload failed, keeping default logo")
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
