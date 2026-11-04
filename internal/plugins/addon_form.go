package plugins

// formJS reads the settings form an add-on's configure page shows (after
// the page's own script has built it) and lets Ether drive it. Ether shows
// the fields in its own form; each change is made on the page's real
// control, so the page itself writes the install link, whatever its code
// does with the values. No add-on is special-cased: the form is read from
// the controls a person would use, whatever the page is built with:
//
//   - native inputs, selects and textareas, checkbox and radio groups
//     (sharing a name or a framework model binding);
//   - form web components (Shoelace and the like): elements with a value
//     or checked property, options from their value-carrying children;
//   - ARIA comboboxes (Material UI, Radix, Headless UI): opened once out of
//     sight to read their options, then closed;
//   - chips: a row of toggle buttons (or image buttons) whose on state is
//     read from ARIA, then dimming, then colour.
//
// Messages to Ether: {source:"ether-addon-relay", type:"form", fields,
// partial}. partial says the page has settings Ether can't show (tabs,
// "Add …" list editors), so its own page is offered alongside.
// Messages from Ether: {source:"ether", type:"set", id, value},
// {type:"commit"} (ask the page for its install link) and {type:"read"}.
//
// A field: {id, kind, label, value, options?, placeholder?, min?, max?,
// step?, set?}. kind is text, number, secret, bool, select or multiSelect.
// Secret values never leave the page; set says whether one is filled in.
const formJS = `(function(){
var SEQ=0,LAST="",T=0,PROBING=false,PROBES={},QUEUE=[],LEARNT={};
function eid(el){var v=el.getAttribute("data-ether-id");if(!v){v="f"+(++SEQ);el.setAttribute("data-ether-id",v)}return v}
function txt(el){return el?String(el.textContent||"").replace(/\s+/g," ").trim():""}
function shown(el){if(!el||!el.isConnected)return false;var s=getComputedStyle(el);if(s.visibility==="hidden"||s.display==="none")return false;return !!(el.offsetWidth||el.offsetHeight||el.getClientRects().length)}
function clean(s){s=String(s||"").replace(/\s+/g," ").trim().replace(/\s*[:*]+$/,"");if(s.length>3&&s===s.toUpperCase()&&/[A-Z]/.test(s))s=s.charAt(0)+s.slice(1).toLowerCase();return s.slice(0,80)}
function wait(ms){return new Promise(function(r){setTimeout(r,ms)})}
var NATIVE="input,select,textarea";
var BTN="button,[role=button],[role=option],[role=checkbox],[role=switch]";
var CTRL=NATIVE+","+BTN+",[role=combobox]";
var LBL="label,legend,h1,h2,h3,h4,h5,h6,[role=heading],dt,th";
var SECRET=/(api.?key|token|secret|passw|password|auth|cookie|credential|client.?secret)/i;
var ACTION=/\b(install|cop(y|ied)|save|generate|cancel|close|submit|donat\w*|support|sponsor|tip|paypal|ko-?fi|patreon|bitcoin|btc|eth|sol|usdt|xmr|reset|share|open|back|next|continue|login|log in|sign in|sign up|dark mode|light mode|theme)\b/i;
function custom(el){return el.tagName.indexOf("-")>0}
// A form web component: has a value or checked property and is not a button.
function isWC(el){if(!custom(el)||/BUTTON|ICON|OPTION|MENU-ITEM|TAB/.test(el.tagName))return false;var v;try{v=el.value}catch(e){return false}if(typeof el.checked==="boolean")return true;return v!==undefined&&v!==null&&(typeof v==="string"||typeof v==="number"||Array.isArray(v))&&(el.hasAttribute("label")||el.hasAttribute("name")||el.id||wcOptions(el).length>0)}
function wcOptions(el){return [].filter.call(el.children,function(c){return c.hasAttribute("value")&&(custom(c)||c.tagName==="OPTION"||c.getAttribute("role")==="option")})}
function wraps(l){return !!l.querySelector(CTRL)}
function model(el){var a=["x-model","v-model","ng-model","data-model","wire:model","formcontrolname"];for(var i=0;i<a.length;i++){var v=el.getAttribute(a[i]);if(v)return a[i]+"="+v}return ""}
function textBlock(n){if(!n||/^(SCRIPT|STYLE|TEMPLATE|SVG|IMG)$/i.test(n.tagName)||n.matches(CTRL)||isWC(n))return null;if(n.matches(LBL)&&!wraps(n)&&seen(n)&&seen(n).length<=60)return n;if(n.querySelector(CTRL))return lastLbl(n);var s=txt(n);return s&&s.length<=60?n:null}
function lastLbl(n){var ls=n.querySelectorAll(LBL);for(var i=ls.length-1;i>=0;i--){if(!wraps(ls[i])&&seen(ls[i])&&seen(ls[i]).length<=60)return ls[i]}return null}
// The label a person reads for a control: its own <label>, aria naming,
// then the nearest text above it, closest first.
function fieldLabel(el,far){
 if(el.id){try{var f=document.querySelector('label[for="'+CSS.escape(el.id)+'"]');if(f&&txt(f))return f}catch(e){}}
 var by=el.getAttribute("aria-labelledby");if(by){var ids=by.split(/\s+/);for(var b=0;b<ids.length;b++){var x=document.getElementById(ids[b]);if(x&&x!==el&&!x.contains(el)&&txt(x))return x}}
 var w=el.closest("label");if(w&&w!==el&&w.querySelectorAll(CTRL).length===1&&txt(w))return w;
 var cur=el;for(var i=0;i<(far?7:3)&&cur&&cur!==document.body;i++){
  for(var s=cur.previousElementSibling;s;s=s.previousElementSibling){var t=textBlock(s);if(t)return t;if(s.matches(CTRL)||isWC(s)||s.querySelector(CTRL))break}
  cur=cur.parentElement}
 return null}
function seen(el){if(!el)return"";var s=shown(el)?String(el.innerText||""):txt(el);var raw=txt(el);if(s===s.toUpperCase()&&raw!==raw.toUpperCase()&&raw.length<=s.length+4)s=raw;return s.replace(/\(\?\)|ⓘ|\?$/g,"").replace(/\s+/g," ").trim()}
// Sentence case, as Ether writes labels; acronyms and names like
// RealDebrid keep their capitals.
function sentence(s){return s.split(" ").map(function(w,i){return i>0&&/^[A-Z][a-z]+$/.test(w)?w.toLowerCase():w}).join(" ")}
function nameOf(el,lab){return sentence(clean(el.getAttribute("label")||(lab&&seen(lab))||el.getAttribute("aria-label")||el.getAttribute("placeholder")||el.getAttribute("name")||el.id||""))}
function labelFor(el){var near=fieldLabel(el,false);if(near)return near;if(el.getAttribute("label")||el.getAttribute("aria-label")||el.getAttribute("placeholder"))return null;return fieldLabel(el,true)}
function prettyFile(src){var m=/([^\/?#]+?)(?:\.[a-z0-9]+)?(?:[?#].*)?$/i.exec(src||"");if(!m)return"";var s=decodeURIComponent(m[1]).replace(/[-_]+/g," ");return s.charAt(0).toUpperCase()+s.slice(1)}
/* ---------------------------------------------------------------- chips */
function chroma(el){var m=/rgba?\(([^)]+)\)/.exec(getComputedStyle(el).backgroundColor||"");if(!m)return 0;var p=m[1].split(",").map(parseFloat);if(p.length>3&&p[3]<0.3)return 0;return Math.max(p[0],p[1],p[2])-Math.min(p[0],p[1],p[2])}
function cls(el){return el.className&&el.className.baseVal!==undefined?el.className.baseVal:String(el.className||"")}
function flagged(el){var a=el.getAttribute("aria-pressed")||el.getAttribute("aria-checked")||el.getAttribute("aria-selected");if(a==="true")return true;if(a==="false")return false;var d=el.getAttribute("data-state")||el.getAttribute("data-active")||el.getAttribute("data-selected");if(d)return /^(on|checked|active|selected|true)$/.test(d);if(/(^|\s)(active|selected|checked|is-active|is-selected|on)(\s|$)/.test(cls(el)))return true;return null}
function dim(item,btn){var o=1,n=btn,f="";while(n&&n!==item.parentElement){var s=getComputedStyle(n);o*=parseFloat(s.opacity||"1");f+=" "+s.filter;n=n.parentElement}return o<0.7||/grayscale\((1|0\.[5-9])|saturate\(0(\.[0-4])?\)|brightness\(0\.[0-6]/.test(f)}
function look(it){var b=chipBtn(it);return cls(b)+"|"+dim(it,b)+"|"+(chroma(b)>60)}
function chipState(items,c0){
 var btns=items.map(chipBtn),out=[],i;
 for(i=0;i<btns.length;i++){var f=flagged(btns[i]);if(f===null)f=flagged(items[i]);if(f===null){out=null;break}out.push(f)}
 if(out)return out;
 var d=items.map(function(it,j){return dim(it,btns[j])});if(d.some(Boolean)&&!d.every(Boolean))return d.map(function(x){return !x});
 var c=btns.map(chroma),hi=c.filter(function(x){return x>60}).length,sig={};btns.forEach(function(b){sig[cls(b)]=1});
 var L=c0&&LEARNT[c0.getAttribute("data-ether-id")];
 if(L)return items.map(function(it){return look(it)===L.look?L.on:!L.on});
 if(hi===0&&Object.keys(sig).length>=1)return null;
 return c.map(function(x){return x>60})}
function btnLike(b){var r=b.getAttribute("role");return b.tagName==="BUTTON"&&b.getAttribute("type")!=="submit"||r==="button"||r==="option"||r==="checkbox"||r==="switch"||r==="radio"}
function chipBtn(it){if(btnLike(it))return it;var bs=it.querySelectorAll(BTN);return bs.length===1&&btnLike(bs[0])?bs[0]:null}
function chipText(it){var b=chipBtn(it);var s=seen(it);if(s)return s;var img=it.querySelector("img");return clean(b.getAttribute("aria-label")||b.getAttribute("title")||(img&&(img.getAttribute("alt")||img.getAttribute("title")))||txt(it)||(img&&prettyFile(img.getAttribute("src"))))}
function chipKids(c){var k=[];for(var i=0;i<c.children.length;i++){var ch=c.children[i];if(/^(TEMPLATE|SCRIPT|STYLE)$/.test(ch.tagName))continue;k.push(ch)}return k}
function isChips(c){if(c.closest("[role=listbox],[role=menu],[role=tablist],nav"))return false;var k=chipKids(c);if(k.length<2)return false;for(var i=0;i<k.length;i++){var b=chipBtn(k[i]);if(!b||k[i].querySelector(NATIVE))return false;var s=chipText(k[i]);if(!s||s.length>40||ACTION.test(s))return false}return true}
/* ------------------------------------------------------------ comboboxes */
function listOf(cb){var id=cb.getAttribute("aria-controls")||cb.getAttribute("aria-owns");var l=id&&document.getElementById(id);if(l&&l.getAttribute("role")==="listbox")return l;var ls=document.querySelectorAll("[role=listbox]");for(var i=ls.length-1;i>=0;i--){if(shown(ls[i]))return ls[i]}return null}
function key(k,el){el.dispatchEvent(new KeyboardEvent("keydown",{key:k,code:k,bubbles:true,cancelable:true}))}
async function open(cb){cb.focus&&cb.focus();cb.dispatchEvent(new MouseEvent("mousedown",{bubbles:true,cancelable:true,button:0}));var l;for(var i=0;i<10;i++){await wait(60);l=listOf(cb);if(l&&shown(l))return l}cb.click();for(i=0;i<8;i++){await wait(60);l=listOf(cb);if(l&&shown(l))return l}key("ArrowDown",cb);for(i=0;i<8;i++){await wait(60);l=listOf(cb);if(l&&shown(l))return l}return null}
async function close(cb,l){key("Escape",l||cb);key("Escape",document.activeElement||cb);await wait(80);if(l&&shown(l))document.body.dispatchEvent(new MouseEvent("mousedown",{bubbles:true,button:0}));await wait(60);cb.blur&&cb.blur()}
// Which options are chosen. Command lists (cmdk) use aria-selected for the
// highlighted row, so there a check mark that shows or hides decides.
// "Add a language", "Select languages you like": a prompt to collect several.
var MANY=/^(add\b|(select|choose|pick)\b.*\b[a-z]{3,}s\b)/i;
function iconOn(o){var i=o.querySelector("svg");if(!i)return null;var n=i,op=1;while(n&&n!==o){op*=parseFloat(getComputedStyle(n).opacity||"1");n=n.parentElement}var s=getComputedStyle(i);return op>0.5&&s.visibility!=="hidden"&&s.display!=="none"}
function optsOf(l){var os=[].slice.call(l.querySelectorAll("[role=option]"));
 var icons=os.map(iconOn),varies=icons.some(function(x){return x===true})&&icons.some(function(x){return x===false});
 var cmdk=os.some(function(o){return o.hasAttribute("cmdk-item")});
 return os.map(function(o,i){var checked=o.getAttribute("aria-checked")==="true"||o.getAttribute("data-state")==="checked"||o.getAttribute("data-checked")==="true";
  var on=checked||(cmdk||varies?(varies&&icons[i]===true):o.getAttribute("aria-selected")==="true");
  return{value:o.getAttribute("data-value")||o.getAttribute("value")||txt(o),label:txt(o),on:on}}).filter(function(o){return o.label})}
async function probe(cb){var l=await open(cb);if(!l)return null;var o=optsOf(l);var multi=l.getAttribute("aria-multiselectable")==="true"||o.filter(function(x){return x.on}).length>1||cb.tagName==="INPUT"&&(MANY.test(cb.getAttribute("placeholder")||"")||MANY.test(txt(fieldLabel(cb,false)))||(!cb.value&&o.some(function(x){return x.on})));await close(cb,l);return{options:o.map(function(x){return{value:x.value,label:x.label}}),value:multi?o.filter(function(x){return x.on}).map(function(x){return x.value}):((o.filter(function(x){return x.on})[0]||{}).value||""),multi:multi}}
// Toggle one chip and back: the look that goes dim or grey is "off".
async function learn(c){var it=chipKids(c).filter(shown)[0];if(!it)return;var b=chipBtn(it);
 var m0={look:look(it),dim:dim(it,b),ch:chroma(b)};b.click();await wait(200);var m1={look:look(it),dim:dim(it,b),ch:chroma(b)};b.click();await wait(200);
 var on0=null;if(m0.dim!==m1.dim)on0=!m0.dim;else if(Math.abs(m0.ch-m1.ch)>40)on0=m0.ch>m1.ch;
 if(on0!==null&&m0.look!==m1.look)LEARNT[eid(c)]={look:m0.look,on:on0};else LEARNT[eid(c)]={failed:true}}
async function runQueue(){if(PROBING)return;PROBING=true;try{while(QUEUE.length){var cb=QUEUE.shift();if(cb.getAttribute("role")!=="combobox"){try{await learn(cb)}catch(e){LEARNT[eid(cb)]={failed:true}}continue}try{PROBES[eid(cb)]=await probe(cb)||{options:[],value:"",multi:false,failed:true}}catch(e){PROBES[eid(cb)]={failed:true}}}}finally{PROBING=false}soon()}
/* --------------------------------------------------------------- reading */
function extract(){
 var out=[],done=new Set(),buckets={},owned=new Set();
 document.querySelectorAll("[role=combobox][aria-controls]").forEach(function(cb){var l=document.getElementById(cb.getAttribute("aria-controls"));if(l)owned.add(l)});
 var all=document.body?document.body.querySelectorAll("*"):[];
 for(var i=0;i<all.length;i++){var el=all[i];if(done.has(el))continue;
  if(el.closest("[aria-hidden=true],[inert],dialog:not([open])"))continue;
  var lb=el.closest("[role=listbox]");if(lb&&(owned.has(lb)||PROBING))continue;
  var tag=el.tagName,type=String(el.type||"").toLowerCase(),role=el.getAttribute("role");
  // Web components first: their insides are theirs.
  if(isWC(el)){el.querySelectorAll("*").forEach(function(n){done.add(n)});var lab0=labelFor(el);if(!(shown(el)||shown(lab0)))continue;
   if(typeof el.checked==="boolean"){out.push({id:eid(el),kind:"bool",label:sentence(clean(el.getAttribute("label")||txt(el)||nameOf(el,lab0))),value:!!el.checked});continue}
   var wo=wcOptions(el),lbl=nameOf(el,lab0);
   if(wo.length){var opts0=wo.map(function(o){return{value:o.getAttribute("value"),label:txt(o)||o.getAttribute("value")}});
    if(el.multiple||Array.isArray(el.value)){var v0=Array.isArray(el.value)?el.value:String(el.value||"").split(" ").filter(Boolean);out.push({id:eid(el),kind:"multiSelect",label:lbl,options:opts0,value:v0})}
    else out.push({id:eid(el),kind:"select",label:lbl,options:opts0,value:String(el.value||"")});continue}
   var wt=String(el.getAttribute("type")||"").toLowerCase();
   if(wt==="password"||SECRET.test(lbl)||SECRET.test(el.id||"")){out.push({id:eid(el),kind:"secret",label:lbl,set:!!el.value});continue}
   if(wt==="number"){out.push({id:eid(el),kind:"number",label:lbl,value:el.value===""?null:Number(el.value)});continue}
   out.push({id:eid(el),kind:"text",label:lbl,value:String(el.value),placeholder:el.getAttribute("placeholder")||""});continue}
  if(!el.matches(CTRL))continue;
  if(tag==="INPUT"&&/^(hidden|submit|button|reset|image|file)$/.test(type))continue;
  if((tag==="INPUT"||tag==="TEXTAREA")&&(el.readOnly||el.disabled||/manifest\.json/.test(el.value)))continue;
  if(role==="combobox"){var lab2=labelFor(el);if(!(shown(el)||shown(lab2)))continue;var id2=eid(el),p=PROBES[id2];
   if(!p){if(QUEUE.indexOf(el)<0)QUEUE.push(el);continue}if(p.failed||!p.options.length)continue;
   out.push({id:id2,kind:p.multi?"multiSelect":"select",label:nameOf(el,lab2),options:p.options,value:p.value});continue}
  if(btnLike(el)||(chipBtn(el.parentElement||el)===el)){
   var c=el.parentElement,item=el;
   for(var up=0;up<3&&c&&!isChips(c);up++){item=c;c=c.parentElement}
   if(!c||!isChips(c))continue;
   var k=chipKids(c);k.forEach(function(it){done.add(it);it.querySelectorAll("*").forEach(function(n){done.add(n)})});
   var vis=k.filter(function(it){return shown(it)});if(vis.length<2)continue;
   var labC=labelFor(c);if(labC&&ACTION.test(seen(labC)))continue;
   var st=chipState(vis,c);if(!st){var L0=LEARNT[eid(c)];if(!L0&&QUEUE.indexOf(c)<0)QUEUE.push(c);continue}var lab=labelFor(c);
   var radio=vis.every(function(it){return chipBtn(it).getAttribute("role")==="radio"})||c.getAttribute("role")==="radiogroup";
   var copts=vis.map(function(it){var t=chipText(it);return{value:t,label:t}}),con=vis.filter(function(it,j){return st[j]}).map(chipText);
   out.push(radio?{id:eid(c),kind:"select",label:nameOf(c,lab),options:copts,value:con[0]||"",chips:true}:{id:eid(c),kind:"multiSelect",label:nameOf(c,lab),options:copts,value:con});continue}
  if(type==="checkbox"||type==="radio"){
   var key2=el.name||model(el);
   if(!key2){var lab1=labelFor(el);if(!(shown(el)||shown(lab1)))continue;
    out.push({id:eid(el),kind:"bool",label:sentence(clean(ownText(el)||nameOf(el,lab1))),value:!!el.checked});done.add(el);continue}
   key2=type+":"+key2;if(!buckets[key2]){buckets[key2]=[];out.push({bucket:key2})}buckets[key2].push(el);done.add(el);continue}
  if(tag!=="INPUT"&&tag!=="SELECT"&&tag!=="TEXTAREA")continue;
  var labN=labelFor(el);if(!(shown(el)||shown(labN)&&tag==="SELECT"))continue;
  var label=nameOf(el,labN);
  if(tag==="SELECT"){var opts=[].map.call(el.options,function(o){return{value:o.value,label:txt(o)||o.value}}).filter(function(o){return o.label});
   if(el.multiple)out.push({id:eid(el),kind:"multiSelect",label:label,options:opts,value:[].filter.call(el.options,function(o){return o.selected}).map(function(o){return o.value})});
   else out.push({id:eid(el),kind:"select",label:label,options:opts,value:el.value});continue}
  var secret=type==="password"||SECRET.test(label)||SECRET.test(el.name||"")||SECRET.test(el.getAttribute("placeholder")||"");
  if(secret){out.push({id:eid(el),kind:"secret",label:label,set:!!el.value,placeholder:el.getAttribute("placeholder")||""});continue}
  if(type==="number"||type==="range"){out.push({id:eid(el),kind:"number",label:label,value:el.value===""?null:Number(el.value),placeholder:el.getAttribute("placeholder")||"",min:el.min===""?undefined:Number(el.min),max:el.max===""?undefined:Number(el.max),step:el.step===""||el.step==="any"?undefined:Number(el.step)});continue}
  out.push({id:eid(el),kind:"text",label:label,value:el.value,placeholder:el.getAttribute("placeholder")||""})}
 if(QUEUE.length)setTimeout(runQueue,0);
 // Checkboxes and radios that share a name or model are one choice.
 return out.map(function(f){if(!f.bucket)return f;var g=buckets[f.bucket],first=g[0],lab=labelFor(first);
  if(!(g.some(shown)||shown(lab)))return null;
  var opts=g.map(function(el){var t=clean(ownText(el));var v=el.value&&el.value!=="on"?el.value:t;eid(el);return{value:v,label:t||v}});
  if(g.length===1&&first.type==="checkbox")return{id:eid(first),kind:"bool",label:sentence(clean(ownText(first)||nameOf(first,lab))),value:!!first.checked};
  var label=nameOf(first,fieldLabel(first.closest("label")||first,true));
  if(first.type==="radio"){var on=g.filter(function(el){return el.checked})[0];return{id:eid(first),kind:"select",label:label,options:opts,value:on?opts[g.indexOf(on)].value:""}}
  return{id:eid(first),kind:"multiSelect",label:label,options:opts,value:g.filter(function(el){return el.checked}).map(function(el){return opts[g.indexOf(el)].value})}
 }).filter(function(f){return f&&f.label})}
function ownText(el){
 if(el.id){try{var f=document.querySelector('label[for="'+CSS.escape(el.id)+'"]');if(f&&txt(f))return txt(f)}catch(e){}}
 var w=el.closest("label");if(w&&txt(w))return txt(w);
 var n=el.nextElementSibling;if(n&&txt(n))return txt(n);
 return el.getAttribute("aria-label")||el.getAttribute("title")||""}
// Settings Ether can't show: tabs (other panels), and list editors.
function partial(){var tl=[].some.call(document.querySelectorAll("[role=tablist]"),function(t){return shown(t)&&t.querySelectorAll("[role=tab]").length>1});if(tl)return true;return [].some.call(document.querySelectorAll("button,[role=button],sl-button,md-text-button,md-outlined-button"),function(b){return shown(b)&&/^\s*(\+\s*)?add\b/i.test(txt(b))&&!/add[- ]?on|to stremio/i.test(txt(b))})}
function send(){if(PROBING)return;var f;try{f=extract()}catch(e){try{console.warn("ether form reader",e&&e.stack||e)}catch(x){}return}var msg={fields:f,partial:partial()};var s=JSON.stringify(msg);if(s===LAST)return;LAST=s;parent.postMessage({source:"ether-addon-relay",type:"form",fields:f,partial:msg.partial},"*")}
// Debounced, but never starved: pages that animate change all the time.
var FIRST=0;function soon(){var now=Date.now();if(T&&now-FIRST>1000)return;if(!T)FIRST=now;clearTimeout(T);T=setTimeout(function(){T=0;send()},200)}
/* --------------------------------------------------------------- writing */
function fire(el,names){names.forEach(function(n){el.dispatchEvent(new Event(n,{bubbles:true,composed:true}))})}
function native(el,v){var proto=el.tagName==="TEXTAREA"?HTMLTextAreaElement.prototype:el.tagName==="SELECT"?HTMLSelectElement.prototype:HTMLInputElement.prototype;var d=Object.getOwnPropertyDescriptor(proto,"value");if(d&&d.set)d.set.call(el,v);else el.value=v;fire(el,["input","change"])}
function members(first){var k=first.name?"name":model(first)?model(first).split("=")[0]:"";if(!k)return[first];var v=k==="name"?first.name:first.getAttribute(k);return[].filter.call(document.querySelectorAll('input[type="'+first.type+'"]'),function(el){return k==="name"?el.name===v:el.getAttribute(k)===v})}
async function set(id,value){var el=document.querySelector('[data-ether-id="'+id+'"]');if(!el)return;
 if(isWC(el)){if(typeof el.checked==="boolean")el.checked=!!value;else if(el.multiple||Array.isArray(el.value))el.value=Array.isArray(el.value)?(value||[]):(value||[]).join(" ");else el.value=value==null?"":String(value);
  var pre=el.tagName.split("-")[0].toLowerCase();fire(el,[pre+"-input",pre+"-change","input","change"]);return}
 if(el.getAttribute("role")==="combobox"){var want=Array.isArray(value)?value:[value];var l=await open(el);if(!l)return;
  var multi=l.getAttribute("aria-multiselectable")==="true"||Array.isArray(value);
  for(var pass=0;pass<60;pass++){var os=[].slice.call((listOf(el)||l).querySelectorAll("[role=option]")),hit=null;
   var cur=optsOf(listOf(el)||l);
   for(var j=0;j<os.length;j++){var o=os[j],v=o.getAttribute("data-value")||o.getAttribute("value")||txt(o),on=!!(cur.filter(function(c){return c.value===v})[0]||{}).on;if(multi?(on!==(want.indexOf(v)>=0)):(!on&&want.indexOf(v)>=0)){hit=o;break}}
   if(!hit)break;hit.click();await wait(80);if(!multi)break;if(!listOf(el)||!shown(listOf(el))){l=await open(el);if(!l)break}}
  if(listOf(el)&&shown(listOf(el)))await close(el,listOf(el));PROBES[id]=await probe(el)||PROBES[id];return}
 if(el.tagName!=="INPUT"&&el.tagName!=="SELECT"&&el.tagName!=="TEXTAREA"){var k=chipKids(el).filter(shown),st=chipState(k,el)||[],w2=Array.isArray(value)?value:[value];
  if(!Array.isArray(value)){for(var r=0;r<k.length;r++){if(chipText(k[r])===value&&!st[r]){chipBtn(k[r]).click()}}return}
  for(var i=0;i<k.length;i++){if(!!st[i]!==(w2.indexOf(chipText(k[i]))>=0)){chipBtn(k[i]).click();await wait(30)}}return}
 var type=String(el.type||"").toLowerCase();
 if(el.tagName==="SELECT"){if(el.multiple){var w=value||[];[].forEach.call(el.options,function(o){o.selected=w.indexOf(o.value)>=0});fire(el,["input","change"])}else native(el,value==null?"":String(value));return}
 if(type==="checkbox"||type==="radio"){var g=members(el);if(g.length===1&&type==="checkbox"&&typeof value==="boolean"){if(el.checked!==value)el.click();return}
  var w3=Array.isArray(value)?value:[value];g.forEach(function(m){var t=clean(ownText(m));var v=m.value&&m.value!=="on"?m.value:t;var on=w3.indexOf(v)>=0;if(m.checked!==on&&(type==="checkbox"||on))m.click()});return}
 native(el,value==null?"":String(value))}
// Asked for the link: press the page's own copy or install control; the
// relay's watcher reports what it makes.
function commit(){var bs=document.querySelectorAll("a,button,[role=button],sl-button,md-filled-button"),pick=null;for(var i=0;i<bs.length;i++){var b=bs[i];if(!shown(b))continue;var s=txt(b)+" "+(b.id||"");if(/copy/i.test(s)&&/link|url|manifest/i.test(s)){pick=b;break}if(!pick&&/install|generate|get link|manifest/i.test(s))pick=b}if(pick)pick.click()}
var CHAIN=Promise.resolve();
window.addEventListener("message",function(e){if(e.source!==parent)return;var d=e.data||{};if(d.source!=="ether")return;
 if(d.type==="set"){CHAIN=CHAIN.then(function(){return set(d.id,d.value)}).catch(function(){}).then(soon)}
 else if(d.type==="commit"){CHAIN=CHAIN.then(function(){commit()}).catch(function(){})}
 else if(d.type==="read"){LAST="";soon()}});
function start(){new MutationObserver(function(ms){for(var i=0;i<ms.length;i++){if(ms[i].attributeName!=="data-ether-id"){soon();return}}}).observe(document.documentElement,{subtree:true,childList:true,attributes:true,characterData:true});document.addEventListener("input",soon,true);document.addEventListener("change",soon,true);soon();setTimeout(soon,1500);setTimeout(soon,4000)}
if(document.readyState==="loading")document.addEventListener("DOMContentLoaded",start);else start();
})();`
