(function($){
	// Router > Advanced Settings > Captive portal (omnicap). Talks to /omnicapgw.cgi (JSON), which saves to
	// uci omnicapgw and restarts omnicap-gw. Secrets are write-only: blank keeps the stored value.
	$.fn.objCaptivePortal = function(){
		var url = window.location.protocol + "//" + window.location.host + "/omnicapgw.cgi";
		var timer = null;
		var T = function(k){ return jQuery.i18n.prop(k); };

		this.onLoad = function(){
			LoadWebPage("html/router/omnicapgw.html");
			$('#lt_btnApply').click(save);
			$('#lt_OCGW_btnFetch').click(function(){ post("action=fetchcert", true); });
			$('#lt_OCGW_btnUploadCert').click(uploadCert);
			$('#lt_OCGW_btnRegister').click(register);
			$('#ocgwCertSource').change(toggleSource);
			$('#ocgwEnable, #ocgwHttps').change(toggleFields);
			$('#ocgwClients').on('click', '.ocgwKick', function(){
				post("action=kick&mac=" + encodeURIComponent($(this).attr('data-mac')), false);
			});
			request("GET", null, null);
			if(timer){ clearInterval(timer); }
			timer = setInterval(function(){
				if(!$('#ocgwEnable').length){ clearInterval(timer); timer = null; return; }   // left the page
				request("GET", null, null, true);
			}, 5000);
		};

		function val(id){ return $.trim($(id).val()); }
		function enc(s){ return encodeURIComponent(s); }

		function validate(){
			if(!$('#ocgwEnable').prop('checked')){ return true; }
			if(!/^https?:\/\/[A-Za-z0-9.-]+(:[0-9]{1,5})?(\/[A-Za-z0-9._~%\/-]*)?$/.test(val('#ocgwPortalUrl'))){ return "lt_OCGW_errUrl"; }
			if(!/^[A-Za-z0-9_.:-]{1,64}$/.test(val('#ocgwNas'))){ return "lt_OCGW_errNas"; }
			if(!/^[A-Za-z0-9.-]{1,253}$/.test(val('#ocgwRadiusServer'))){ return "lt_OCGW_errServer"; }
			var ports = [val('#ocgwAuthPort'), val('#ocgwAcctPort'), val('#ocgwHttpsPort')];
			for(var i = 0; i < ports.length; i++){
				if(!/^[0-9]{1,5}$/.test(ports[i]) || +ports[i] < 1 || +ports[i] > 65535){ return "lt_OCGW_errPort"; }
			}
			if($('#ocgwHttps').prop('checked') && !/^[A-Za-z0-9.-]{1,253}$/.test(val('#ocgwLoginHost'))){ return "lt_OCGW_errHost"; }
			if($('#ocgwHttps').prop('checked') && $('#ocgwCertSource').val() == "agent"
				&& !/^https?:\/\/[A-Za-z0-9.-]+(:[0-9]{1,5})?(\/[A-Za-z0-9._~%\/-]*)?$/.test(val('#ocgwAgentUrl'))){ return "lt_OCGW_errAgentUrl"; }
			if(!$('.ocgwPort:checked').length){ return "lt_OCGW_errPorts"; }
			var s = val('#ocgwSecret');
			if(s !== "" && !/^[A-Za-z0-9!#$%()*+,.\/:;<=>?@^_{|}~-]{8,128}$/.test(s)){ return "lt_OCGW_errSecret"; }
			return true;
		}

		function save(){
			var v = validate();
			if(v !== true){ showMsgBox(T("lt_OCGW_title"), T(v)); return; }
			var data = "enabled=" + ($('#ocgwEnable').prop('checked') ? 1 : 0)
				+ "&portal_url=" + enc(val('#ocgwPortalUrl'))
				+ "&nas_identifier=" + enc(val('#ocgwNas'))
				+ "&radius_server=" + enc(val('#ocgwRadiusServer'))
				+ "&radius_auth_port=" + enc(val('#ocgwAuthPort'))
				+ "&radius_acct_port=" + enc(val('#ocgwAcctPort'))
				+ "&radius_secret=" + enc(val('#ocgwSecret'))
				+ "&https=" + ($('#ocgwHttps').prop('checked') ? 1 : 0)
				+ "&login_host=" + enc(val('#ocgwLoginHost'))
				+ "&https_port=" + enc(val('#ocgwHttpsPort'))
				+ "&cert_source=" + enc($('#ocgwCertSource').val())
				+ "&agent_url=" + enc(val('#ocgwAgentUrl'))
				+ "&agent_token=" + enc(val('#ocgwAgentToken'))
				+ "&walled_garden=" + enc(val('#ocgwWalled'))
				+ "&bypass_mac=" + enc(val('#ocgwBypass'))
				+ "&exclude_if=" + enc($('.ocgwPort').not(':checked').map(function(){ return this.value; }).get().join(","));
			post(data, true);
		}

		function register(){
			var u = val('#ocgwInstallUrl'), id = val('#ocgwIdentity');
			if(!/^https?:\/\/[A-Za-z0-9.-]+(:[0-9]{1,5})?\/agent\/v1\/install\/[0-9a-fA-F]{16,128}\/?$/.test(u)){
				showMsgBox(T("lt_OCGW_title"), T("lt_OCGW_errInstallUrl")); return;
			}
			if(!/^[A-Za-z0-9_.-]{1,64}$/.test(id)){ showMsgBox(T("lt_OCGW_title"), T("lt_OCGW_errIdentity")); return; }
			request("POST", "action=register&install_url=" + enc(u) + "&identity=" + enc(id), function(ok, d){
				if(ok){
					$('#ocgwInstallUrl').val("");
					showMsgBox(T("lt_OCGW_title"), T("lt_OCGW_registered") + " " + d.register.nas_identifier);
				} else if(d && d.error == "admin_lockout"){
					showMsgBox(T("lt_OCGW_title"), T("lt_OCGW_errLockout") + " " + d.mac);
				} else {
					var why = d && d.register && d.register.error ? d.register.error : (d && d.error ? d.error : "");
					showMsgBox(T("lt_OCGW_title"), T("lt_OCGW_registerFail") + (why ? " " + why : ""));
				}
			});
		}

		function uploadCert(){
			var c = $('#ocgwCertPem').val(), k = $('#ocgwKeyPem').val();
			if(c.indexOf("BEGIN CERTIFICATE") < 0 || k.indexOf("PRIVATE KEY") < 0){
				showMsgBox(T("lt_OCGW_title"), T("lt_OCGW_errPem")); return;
			}
			post("action=cert&cert_pem=" + enc(c) + "&key_pem=" + enc(k), true);
		}

		function post(data, notify){
			request("POST", data, function(ok, d){
				$('#ocgwSecret, #ocgwAgentToken').val("");
				if(ok){ $('#ocgwCertPem, #ocgwKeyPem').val(""); }
				if(notify){
					var errKey = { portal_url: "lt_OCGW_errUrl", nas_identifier: "lt_OCGW_errNas", radius_server: "lt_OCGW_errServer",
						radius_auth_port: "lt_OCGW_errPort", radius_acct_port: "lt_OCGW_errPort", https_port: "lt_OCGW_errPort",
						radius_secret: "lt_OCGW_errSecret", login_host: "lt_OCGW_errHost", agent_url: "lt_OCGW_errAgentUrl",
						agent_token: "lt_OCGW_errToken", walled_garden: "lt_OCGW_errWalled", bypass_mac: "lt_OCGW_errBypass", exclude_if: "lt_OCGW_errPorts",
						cert: "lt_OCGW_errPem", key: "lt_OCGW_errPem" }[d && d.error];
					var msg = ok ? T("lt_succ") : errKey ? T(errKey)
						: d && d.error == "admin_lockout" ? T("lt_OCGW_errLockout") + " " + d.mac
						: (T("lt_fail") + (d && d.error ? " (" + d.error + ")" : ""));
					showMsgBox(T("lt_OCGW_title"), msg);
				}
			});
		}

		function toggleSource(){
			var agent = $('#ocgwCertSource').val() == "agent";
			$('#ocgwAgentBox').toggle(agent);
			$('#ocgwManualBox').toggle(!agent);
			$('#lt_OCGW_btnFetch').toggle(agent);
		}

		function toggleFields(){
			var on = $('#ocgwEnable').prop('checked');
			// ports and bypass MACs stay editable with the portal off: they are set before registering, which
			// turns the portal on (otherwise the technician's own port would be captured)
			$('#Content input[type=text], #Content input[type=password], #Content select, #ocgwHttps')
				.not('#ocgwEnable, #ocgwInstallUrl, #ocgwIdentity, #ocgwBypass').prop('disabled', !on);
			$('#ocgwLoginHost, #ocgwHttpsPort, #ocgwCertSource, #ocgwAgentUrl, #ocgwAgentToken')
				.prop('disabled', !on || !$('#ocgwHttps').prop('checked'));
		}

		function fmtBytes(n){
			if(n > 1073741824){ return (n / 1073741824).toFixed(2) + " GB"; }
			if(n > 1048576){ return (n / 1048576).toFixed(1) + " MB"; }
			return Math.round(n / 1024) + " KB";
		}

		function renderStatus(d){
			var st = d.status || {};
			var txt;
			if(d.enabled != 1){ txt = T("lt_OCGW_stOff"); }
			else if(!st.running){ txt = T("lt_OCGW_stDown"); }
			else{
				txt = T("lt_OCGW_stOn") + " · " + T("lt_OCGW_stLogin") + " " + st.login_url;
				if(st.radius_error){ txt += " · RADIUS: " + st.radius_error; }
				var pl = st.pull || {};
				if(pl.last){
					txt += " · " + T("lt_OCGW_pull") + " " + (pl.ok ? T("lt_OCGW_pullOk") + " " + pl.interval_s + " s" : T("lt_OCGW_pullErr"));
				}
			}
			$('#ocgwState').text(txt);
			var c = st.cert || {};
			$('#ocgwCert').text(c.loaded
				? (c.subject + " · " + T("lt_OCGW_certIssuer") + " " + c.issuer + " · " + T("lt_OCGW_certUntil") + " " + (c.not_after || "").substr(0, 10)
					+ (c.error ? " · " + c.error : ""))
				: (c.error || T("lt_OCGW_certNone")));
			var rows = "";
			$.each(st.sessions || [], function(i, s){
				rows += "<tr><td>" + s.mac + "</td><td>" + s.ip + "</td><td>" + $('<i>').text(s.user).html()
					+ "</td><td>" + s.minutes + " min / " + s.left_minutes + " min</td><td>↑" + fmtBytes(s.in) + " ↓" + fmtBytes(s.out)
					+ "</td><td>" + s.up_kbit + "/" + s.down_kbit + " kbit/s</td><td><input type='button' class='btn-apply ocgwKick' data-mac='"
					+ s.mac + "' value='" + T("lt_OCGW_kick") + "'></td></tr>";
			});
			$('#ocgwClients').html(rows || "<tr><td colspan='7'>" + T("lt_OCGW_noClients") + "</td></tr>");
		}

		// One checkbox per LAN bridge port the device reports (main and guest SSIDs, cable, USB); checked =
		// captured by the portal. The page only sends the unchecked ones (exclude_if).
		function portLabel(p){
			var name;
			if(p.kind == "wifi"){
				name = p.band == "2g" ? T("lt_OCGW_portWifi24") : p.band == "5g" ? T("lt_OCGW_portWifi5") : "Wi-Fi";
				if(p.guest){ name += " · " + T("lt_OCGW_portGuest"); }
				name += " · " + p.ssid;
				if(!p.enabled){ name += " (" + T("lt_OCGW_portOff") + ")"; }
			}
			else if(p.kind == "cable"){ name = T("lt_OCGW_portLan"); }
			else if(p.kind == "usb"){ name = T("lt_OCGW_portUsb"); }
			else{ name = p["if"]; }
			return name;
		}

		function renderPorts(d){
			var ex = d.exclude_if ? d.exclude_if.split(",") : [];
			var box = $('#ocgwPorts').empty();
			$.each(d.lan_ports || [], function(i, p){
				var cb = $('<input type="checkbox" class="ocgwPort">').val(p["if"]).prop('checked', ex.indexOf(p["if"]) < 0);
				box.append($('<label style="display:block;margin:2px 0;color:inherit;font-weight:normal">').append(cb, " ",
					$('<span>').text(portLabel(p)), " ", $('<span style="color:#999;font-size:11px">').text(p["if"])));
			});
			toggleFields();
		}

		function render(d, statusOnly){
			renderStatus(d);
			if(statusOnly){ return; }
			$('#ocgwEnable').prop('checked', d.enabled == 1);
			$('#ocgwPortalUrl').val(d.portal_url);
			$('#ocgwNas').val(d.nas_identifier);
			$('#ocgwRadiusServer').val(d.radius_server);
			$('#ocgwAuthPort').val(d.radius_auth_port);
			$('#ocgwAcctPort').val(d.radius_acct_port);
			$('#ocgwSecret').attr('placeholder', d.secret_set ? T("lt_OCGW_saved") : "");
			$('#ocgwHttps').prop('checked', d.https == 1);
			$('#ocgwLoginHost').val(d.login_host);
			$('#ocgwHttpsPort').val(d.https_port);
			$('#ocgwCertSource').val(d.cert_source || "agent");
			$('#ocgwAgentUrl').val(d.agent_url);
			$('#ocgwAgentToken').attr('placeholder', d.token_set ? T("lt_OCGW_saved") : "");
			$('#ocgwWalled').val(d.walled_garden);
			$('#ocgwBypass').val(d.bypass_mac);
			renderPorts(d);
			toggleSource();
			toggleFields();
		}

		function request(method, data, done, statusOnly){
			$.ajax({
				type: method, url: url, data: data,
				contentType: "application/x-www-form-urlencoded", dataType: "json", cache: false,
				timeout: 60000, headers: { "X-Requested-With": "XMLHttpRequest" },
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
