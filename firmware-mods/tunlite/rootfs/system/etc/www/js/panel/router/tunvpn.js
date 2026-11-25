(function($){
	// Router > Advanced Settings > Management VPN (OpenVPN over tunlite). Talks to /tunvpn.cgi (JSON), which
	// saves to uci tunvpn and restarts openvpn. The password is write-only: blank keeps the stored value.
	$.fn.objMgmtVPN = function(){
		var url = window.location.protocol + "//" + window.location.host + "/tunvpn.cgi";
		var timer = null;
		var T = function(k){ return jQuery.i18n.prop(k); };

		this.onLoad = function(){
			LoadWebPage("html/router/tunvpn.html");
			$('#lt_btnApply').click(save);
			$('#lt_TVPN_btnReconnect').click(function(){ post("action=restart", true); });
			$('#lt_TVPN_btnCa').click(function(){
				var c = $('#tvpnCaPem').val();
				if(c.indexOf("BEGIN CERTIFICATE") < 0 || c.indexOf("PRIVATE KEY") >= 0){
					showMsgBox(T("lt_TVPN_title"), T("lt_TVPN_errCa")); return;
				}
				post("action=ca&ca_pem=" + encodeURIComponent(c), true);
			});
			$('#tvpnEnable').change(toggleFields);
			request("GET", null, null);
			if(timer){ clearInterval(timer); }
			timer = setInterval(function(){
				if(!$('#tvpnEnable').length){ clearInterval(timer); timer = null; return; }
				request("GET", null, null, true);
			}, 5000);
		};

		function val(id){ return $.trim($(id).val()); }

		function save(){
			var rem = val('#tvpnRemote'), port = val('#tvpnPort');
			if($('#tvpnEnable').prop('checked') && !/^[A-Za-z0-9.-]{1,253}$/.test(rem)){ showMsgBox(T("lt_TVPN_title"), T("lt_TVPN_errServer")); return; }
			if(!/^[0-9]{1,5}$/.test(port) || +port < 1 || +port > 65535){ showMsgBox(T("lt_TVPN_title"), T("lt_TVPN_errPort")); return; }
			var u = val('#tvpnUser');
			if(u !== "" && !/^[A-Za-z0-9._@-]{1,64}$/.test(u)){ showMsgBox(T("lt_TVPN_title"), T("lt_TVPN_errUser")); return; }
			var data = "enabled=" + ($('#tvpnEnable').prop('checked') ? 1 : 0)
				+ "&remote=" + encodeURIComponent(rem) + "&port=" + port
				+ "&proto=" + $('#tvpnProto').val()
				+ "&username=" + encodeURIComponent(u)
				+ "&password=" + encodeURIComponent(val('#tvpnPass'))
				+ "&cipher=" + encodeURIComponent($('#tvpnCipher').val())
				+ "&auth=" + encodeURIComponent($('#tvpnAuth').val())
				+ "&route_nopull=" + ($('#tvpnNoPull').prop('checked') ? 1 : 0);
			post(data, true);
		}

		function post(data, notify){
			request("POST", data, function(ok, d){
				$('#tvpnPass').val("");
				if(ok){ $('#tvpnCaPem').val(""); }
				if(notify){
					showMsgBox(T("lt_TVPN_title"), ok ? T("lt_succ") : (T("lt_fail") + (d && d.error ? " (" + d.error + ")" : "")));
				}
			});
		}

		function toggleFields(){
			var on = $('#tvpnEnable').prop('checked');
			$('#tvpnRemote, #tvpnPort, #tvpnProto, #tvpnUser, #tvpnPass, #tvpnCipher, #tvpnAuth, #tvpnNoPull').prop('disabled', !on);
		}

		function fmtBytes(n){
			if(n > 1048576){ return (n / 1048576).toFixed(1) + " MB"; }
			return Math.round(n / 1024) + " KB";
		}

		function render(d, statusOnly){
			var s = d.status || {};
			var txt;
			if(d.enabled != 1){ txt = T("lt_TVPN_stOff"); }
			else if(!s.module){ txt = T("lt_TVPN_stNoModule"); }
			else if(!s.running){ txt = T("lt_TVPN_stDown"); }
			else if(s.connected && s.ip){ txt = T("lt_TVPN_stOn") + " " + s.ip + " · " + T("lt_TVPN_since") + " " + s.since + " · ↓" + fmtBytes(s.rx) + " ↑" + fmtBytes(s.tx); }
			else { txt = T("lt_TVPN_stConnecting"); }
			$('#tvpnState').text(txt);
			$('#tvpnCaState').text(d.ca_set ? T("lt_TVPN_caSet") : T("lt_TVPN_caNone"));
			$('#tvpnLog').text((s.log || []).join("\n"));
			if(statusOnly){ return; }
			$('#tvpnEnable').prop('checked', d.enabled == 1);
			$('#tvpnRemote').val(d.remote);
			$('#tvpnPort').val(d.port);
			$('#tvpnProto').val(d.proto);
			$('#tvpnUser').val(d.username);
			$('#tvpnPass').attr('placeholder', d.password_set ? T("lt_TVPN_saved") : "");
			$('#tvpnCipher').val(d.cipher);
			$('#tvpnAuth').val(d.auth);
			$('#tvpnNoPull').prop('checked', d.route_nopull == 1);
			toggleFields();
		}

		function request(method, data, done, statusOnly){
			$.ajax({
				type: method, url: url, data: data,
				contentType: "application/x-www-form-urlencoded", dataType: "json", cache: false,
				timeout: 30000, headers: { "X-Requested-With": "XMLHttpRequest" },
				success: function(d){
					if(d && d.result == "ok"){ render(d, statusOnly); }
					if(done){ done(d && d.result == "ok", d); }
				},
				error: function(xhr){
					if(xhr.status == 401){ clearAuthheader(); return; }
					var d = null; try { d = JSON.parse(xhr.responseText); } catch(e){}
					if(done){ done(false, d); }
				}
			});
		}
		return this;
	};
})(jQuery);
