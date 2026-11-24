(function($){
	// Outbound TTL setting, shown in the UI as "Network parameter" (the UI text never says TTL).
	// It is not a menu entry of its own: it is appended as a section at the bottom of the factory
	// Diagnostics page (Router > System Settings > Diagnostics). Talks to /notion_ttl.cgi (JSON), which
	// saves to uci notion_ttl and re-applies the mangle rules. See OEM-SPEC.md.
	$.fn.objNotionTTL = function(){
		var url = window.location.protocol + "//" + window.location.host + "/notion_ttl.cgi";

		// Append the section to the page currently loaded in #Content (called after Diagnostics loads).
		this.attach = function(){
			$('#NotionTTLSection').remove();
			$('<div id="NotionTTLSection" style="clear:both;margin-top:30px"></div>')
				.html(CallHtmlFile("html/router/notion_ttl.html"))
				.appendTo('#Content');
			LocalAllElement();
			init();
		};

		function init(){
			$('#lt_btnApply').click(function(){
				var ttl = $.trim($('#txtNotionTTL').val());
				if(!/^[0-9]{1,3}$/.test(ttl) || parseInt(ttl, 10) < 1 || parseInt(ttl, 10) > 255){
					showMsgBox(jQuery.i18n.prop("lt_NotionTTL_title"), jQuery.i18n.prop("lt_NotionTTL_invalid"));
					return;
				}
				var data = "enabled=" + ($('#NotionTTLEnable').prop('checked') ? "1" : "0")
					+ "&ttl=" + parseInt(ttl, 10)
					+ "&ipv6=" + ($('#NotionTTLIpv6').prop('checked') ? "1" : "0");
				request("POST", data, function(ok){
					showMsgBox(jQuery.i18n.prop("lt_NotionTTL_title"),
						jQuery.i18n.prop(ok ? "lt_succ" : "lt_fail"));
				});
			});

			// "Recommended value" = action=restore (enabled, 64, IPv6). "Factory behavior" = action=remove
			// (disable and delete the rules, as the factory firmware behaves).
			// Both ask first, with the firmware's own Yes/No dialog (#confirmDlg, the one logout uses).
			$('#lt_NotionTTL_btnRestore').click(function(){ confirmThen("lt_NotionTTL_confirmRestore", "action=restore"); });
			$('#lt_NotionTTL_btnRemove').click(function(){ confirmThen("lt_NotionTTL_confirmRemove", "action=remove"); });

			$('#NotionTTLEnable').change(toggleFields);
			request("GET", null, null);
		}

		function confirmThen(msgKey, data){
			ShowDlg("confirmDlg", 400, 160);
			// ShowDlg copies the template into #mbd, so bind on that copy.
			$("#mbd #lt_confirmDlg_title").text(jQuery.i18n.prop("lt_NotionTTL_title"));
			$("#mbd #lt_confirmDlg_msg").text(jQuery.i18n.prop(msgKey));
			$("#mbd #lt_btnConfirmYes").val(jQuery.i18n.prop("lt_btnConfirmYes")).click(function(){
				CloseDlg();
				request("POST", data, function(ok){
					showMsgBox(jQuery.i18n.prop("lt_NotionTTL_title"),
						jQuery.i18n.prop(ok ? "lt_succ" : "lt_fail"));
				});
			});
			$("#mbd #lt_btnConfirmNo").val(jQuery.i18n.prop("lt_btnConfirmNo"));
		}

		function toggleFields(){
			var on = $('#NotionTTLEnable').prop('checked');
			$('#txtNotionTTL').prop('disabled', !on);
			$('#NotionTTLIpv6').prop('disabled', !on);
		}

		function render(d){
			$('#NotionTTLEnable').prop('checked', d.enabled == 1);
			$('#txtNotionTTL').val(d.ttl);
			$('#NotionTTLIpv6').prop('checked', d.ipv6 == 1);
			toggleFields();
			// The status word lives in lt_* elements so LocalAllElement() re-translates it when the
			// language changes; here we only pick which one is shown and fill in the numbers.
			var on = d.enabled == 1;
			$('#lt_NotionTTL_on').text(jQuery.i18n.prop("lt_NotionTTL_on")).toggle(on);
			$('#lt_NotionTTL_off').text(jQuery.i18n.prop("lt_NotionTTL_off")).toggle(!on);
			$('#NotionTTLStatusVal').text(!on ? "" : d.ttl
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
		if(!orig || orig.notionTTL){ return; }
		$.fn.objDiagnostics = function(){
			var panel = orig.apply(this, arguments);
			var load = panel.onLoad;
			panel.onLoad = function(flag){
				var r = load.apply(this, arguments);
				if(flag){ $(document).objNotionTTL().attach(); }
				return r;
			};
			return panel;
		};
		$.fn.objDiagnostics.notionTTL = true;
	});
})(jQuery);
