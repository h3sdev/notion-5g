(function($){
	// Página "TTL" (Router > Configuración avanzada). Habla con /notion_ttl.cgi (JSON), que guarda en
	// uci notion_ttl y re-aplica las reglas mangle; ver firmware-mods/notion-ttl/ en el repo.
	$.fn.objNotionTTL = function(){
		var url = window.location.protocol + "//" + window.location.host + "/notion_ttl.cgi";

		this.onLoad = function(){
			LoadWebPage("html/router/notion_ttl.html");

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

			$('#NotionTTLEnable').change(toggleFields);
			request("GET", null, null);
		};

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
			// La palabra de estado va en elementos lt_* para que LocalAllElement() la retraduzca al cambiar
			// de idioma; aquí solo se elige cuál se muestra y se ponen los números.
			var on = d.enabled == 1;
			$('#lt_NotionTTL_on').text(jQuery.i18n.prop("lt_NotionTTL_on")).toggle(on);
			$('#lt_NotionTTL_off').text(jQuery.i18n.prop("lt_NotionTTL_off")).toggle(!on);
			$('#NotionTTLStatusVal').text(!on ? "" : d.ttl + " (" + d.iface + ")"
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
					if(xhr.status == 401){ clearAuthheader(); return; }   // sesión vencida -> login
					if(done){ done(false); }
				}
			});
		}
		return this;
	};
})(jQuery);
