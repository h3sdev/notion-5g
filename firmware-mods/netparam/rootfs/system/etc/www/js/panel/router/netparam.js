(function($){
	// "Network parameter" section (see OEM-SPEC.md).
	// It is not a menu entry of its own: it is appended as a section at the bottom of the factory
	// Diagnostics page (Router > System Settings > Diagnostics). Talks to /netparam.cgi (JSON), which
	// saves to uci netparam and re-applies the firewall rules. See OEM-SPEC.md.
	$.fn.objNetParam = function(){
		var url = window.location.protocol + "//" + window.location.host + "/netparam.cgi";

		// Append the section to the page currently loaded in #Content (called after Diagnostics loads).
		this.attach = function(){
			$('#NetParamSection').remove();
			$('<div id="NetParamSection" style="clear:both;margin-top:30px"></div>')
				.html(CallHtmlFile("html/router/netparam.html"))
				.appendTo('#Content');
			LocalAllElement();
			init();
		};

		function init(){
			$('#lt_btnApply').click(function(){
				var val = $.trim($('#txtNetParam').val());
				if(!/^[0-9]{1,3}$/.test(val) || parseInt(val, 10) < 1 || parseInt(val, 10) > 255){
					showMsgBox(jQuery.i18n.prop("lt_NetParam_title"), jQuery.i18n.prop("lt_NetParam_invalid"));
					return;
				}
				var data = "enabled=" + ($('#NetParamEnable').prop('checked') ? "1" : "0")
					+ "&value=" + parseInt(val, 10)
					+ "&ipv6=" + ($('#NetParamIpv6').prop('checked') ? "1" : "0");
				request("POST", data, function(ok){
					showMsgBox(jQuery.i18n.prop("lt_NetParam_title"),
						jQuery.i18n.prop(ok ? "lt_succ" : "lt_fail"));
				});
			});

			// "Recommended value" = action=restore (enabled, 64, IPv6). "Factory behavior" = action=remove
			// (disable and delete the rules, as the factory firmware behaves).
			// Both ask first, with the firmware's own Yes/No dialog (#confirmDlg, the one logout uses).
			$('#lt_NetParam_btnRestore').click(function(){ confirmThen("lt_NetParam_confirmRestore", "action=restore"); });
			$('#lt_NetParam_btnRemove').click(function(){ confirmThen("lt_NetParam_confirmRemove", "action=remove"); });

			$('#NetParamEnable').change(toggleFields);
			request("GET", null, null);
		}

		function confirmThen(msgKey, data){
			ShowDlg("confirmDlg", 400, 160);
			// ShowDlg copies the template into #mbd, so bind on that copy.
			$("#mbd #lt_confirmDlg_title").text(jQuery.i18n.prop("lt_NetParam_title"));
			$("#mbd #lt_confirmDlg_msg").text(jQuery.i18n.prop(msgKey));
			$("#mbd #lt_btnConfirmYes").val(jQuery.i18n.prop("lt_btnConfirmYes")).click(function(){
				CloseDlg();
				request("POST", data, function(ok){
					showMsgBox(jQuery.i18n.prop("lt_NetParam_title"),
						jQuery.i18n.prop(ok ? "lt_succ" : "lt_fail"));
				});
			});
			$("#mbd #lt_btnConfirmNo").val(jQuery.i18n.prop("lt_btnConfirmNo"));
		}

		function toggleFields(){
			var on = $('#NetParamEnable').prop('checked');
			$('#txtNetParam').prop('disabled', !on);
			$('#NetParamIpv6').prop('disabled', !on);
		}

		function render(d){
			$('#NetParamEnable').prop('checked', d.enabled == 1);
			$('#txtNetParam').val(d.value);
			$('#NetParamIpv6').prop('checked', d.ipv6 == 1);
			toggleFields();
			// The status word lives in lt_* elements so LocalAllElement() re-translates it when the
			// language changes; here we only pick which one is shown and fill in the numbers.
			var on = d.enabled == 1;
			$('#lt_NetParam_on').text(jQuery.i18n.prop("lt_NetParam_on")).toggle(on);
			$('#lt_NetParam_off').text(jQuery.i18n.prop("lt_NetParam_off")).toggle(!on);
			$('#NetParamStatusVal').text(!on ? "" : d.value
				+ " · IPv4: " + (d.active_v4 ? d.pkts_v4 + " pkts" : "-")
				+ " · IPv6: " + (d.active_v6 ? d.pkts_v6 + " pkts" : "-"));
		}

		function request(method, data, done){
			$.ajax({
				type: method,
				url: url,
				data: data,
				contentType: "application/x-www-form-urlencoded",
				dataType: "json",
				cache: false,
				timeout: 15000,
				headers: { "X-Requested-With": "XMLHttpRequest" },
				success: function(d){
					if(d && d.result == "ok"){ render(d); }
					if(done){ done(d && d.result == "ok"); }
				},
				error: function(xhr){
					if(xhr.status == 401){ clearAuthheader(); return; }   // session expired -> login
					if(done){ done(false); }
				}
			});
		}
		return this;
	};

	// Hook into the factory Diagnostics panel without editing it: wrap $.fn.objDiagnostics so that,
	// after its own onLoad renders the ping/nslookup/traceroute form, our section is appended below.
	// Done on document ready so it works whatever the order of the <script> tags in index.html.
	$(function(){
		var orig = $.fn.objDiagnostics;
		if(!orig || orig.netParam){ return; }
		$.fn.objDiagnostics = function(){
			var panel = orig.apply(this, arguments);
			var load = panel.onLoad;
			panel.onLoad = function(flag){
				var r = load.apply(this, arguments);
				if(flag){ $(document).objNetParam().attach(); }
				return r;
			};
			return panel;
		};
		$.fn.objDiagnostics.netParam = true;
	});
})(jQuery);
