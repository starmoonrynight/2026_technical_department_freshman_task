"use strict";
const $ = (id) => document.getElementById(id);
const state = {user:null,config:null,type:"",page:1,total:0,images:[],editing:null,uploading:false,recognizing:false,saving:false,searchVersion:0,composeVersion:0};
const typeNames={lost:"丢失帖",found:"找到帖"};
const statusNames={searching:"寻找中",found:"已取得联系",closed:"已完成"};
let toastTimer;
function toast(message){$("toast").textContent=message;$("toast").hidden=false;clearTimeout(toastTimer);toastTimer=setTimeout(()=>{$("toast").hidden=true;},5000);}
function el(tag,text,className){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(className)node.className=className;return node;}
function button(text,fn,className){const b=el("button",text,className);b.type="button";b.addEventListener("click",()=>Promise.resolve(fn()).catch(e=>toast(e.message)));return b;}
function date(ts){return ts?new Date(ts*1000).toLocaleString("zh-CN",{month:"short",day:"numeric",hour:"2-digit",minute:"2-digit"}):"未填写";}
async function api(path,{method="GET",body}={}){
 const options={method,credentials:"same-origin",headers:{}};
 if(body instanceof FormData)options.body=body;
 else if(body!==undefined){options.headers["Content-Type"]="application/json";options.body=JSON.stringify(body);}
 const res=await fetch(path,options);const data=await res.json().catch(()=>({message:"服务器返回了无效响应"}));
 if(!res.ok){const error=new Error(data.message||"请求失败");error.status=res.status;throw error;}
 return data.data;
}
function renderAccount(){
 $("account").textContent=state.user?state.user.username:"";$("login-button").hidden=!!state.user;
 $("logout-button").hidden=!state.user;$("notifications-button").hidden=!state.user;
 if(!state.user)$("unread").textContent="";
}
async function loadFeed(){
 const version=++state.searchVersion;$("feed").replaceChildren(el("div","正在寻找线索…","empty"));
 const params=new URLSearchParams({q:$("query").value,breadth:$("breadth").value,type:state.type,status:$("status-filter").value,page:state.page,page_size:12});
 try{
  const result=await api("/api/items?"+params);if(version!==state.searchVersion)return;state.total=result.total;
  $("result-count").textContent=result.total+" 条线索";$("feed").replaceChildren();
  if(!result.items.length)$("feed").append(el("div","暂时没有符合条件的线索，试试更宽泛的搜索范围。","empty"));
  for(const item of result.items){
   const card=el("article",undefined,"card");card.tabIndex=0;card.setAttribute("role","button");card.setAttribute("aria-label","查看 "+item.name);
   if(item.images.length){const img=el("img");img.src=item.images[0].url;img.alt=item.name;img.loading="lazy";card.append(img);}else card.append(el("div","暂无物品照片","placeholder"));
   const body=el("div",undefined,"card-body");body.append(el("span",typeNames[item.type]||item.type,"badge "+item.type),el("span",statusNames[item.status]||item.status,"badge neutral"),el("h3",item.name),el("p","地点 · "+item.location),el("p",item.description||"发布者还没有填写补充描述。","description"),el("small","发布于 "+date(item.created_at)));
   card.append(body);card.addEventListener("click",()=>openDetail(item.id).catch(e=>toast(e.message)));card.addEventListener("keydown",e=>{if(e.key==="Enter"||e.key===" "){e.preventDefault();openDetail(item.id).catch(e=>toast(e.message));}});
   $("feed").append(card);
  }
  const pages=Math.max(1,Math.ceil(result.total/12));$("page-label").textContent=state.page+" / "+pages;$("previous-page").disabled=state.page<=1;$("next-page").disabled=state.page>=pages;
 }catch(e){if(version===state.searchVersion){$("feed").replaceChildren(el("div",e.message,"empty"));$("result-count").textContent="";}}
}
function detailRow(label,value){const row=el("div",undefined,"detail-row");row.append(el("strong",label),el("span",value||"未填写"));return row;}
async function openDetail(id){
 const item=await api("/api/items/"+id);const content=$("detail-content");content.replaceChildren();
 content.append(el("span",typeNames[item.type],"badge "+item.type),el("span",statusNames[item.status],"badge neutral"),el("h3",item.name,"detail-title"));
 const images=el("div",undefined,"detail-images");for(const m of item.images){const img=el("img");img.src=m.url;img.alt=item.name;images.append(img);}content.append(images);
 content.append(detailRow("地点",item.location),detailRow("事件时间",date(item.occurred_at)),detailRow("类别 / 外观",[item.category,item.color,item.brand].filter(Boolean).join(" · ")),detailRow("特征标签",item.tags.join("、")),detailRow("描述",item.description),detailRow("联系方式",state.user?(item.contact||"未留下联系方式"):"登录后可查看（如发布者有填写）"),detailRow("AI 自动匹配",item.allow_ai?"已同意参与":"未参与"));
 if(!state.user)content.append(button("登录查看联系方式",()=>{$("detail-dialog").close();$("auth-dialog").showModal();}));
 if(state.user&&state.user.id===item.user_id){
  const actions=el("div",undefined,"actions");
  actions.append(button("编辑",()=>{$("detail-dialog").close();openCompose(item);}));
  if(item.status!=="closed"){const next=item.status==="searching"?"found":"closed";actions.append(button(next==="found"?"标记已取得联系":"标记已完成",async()=>{await api("/api/items/"+id+"/status",{method:"PATCH",body:{status:next}});await openDetail(id);await loadFeed();}));}
  actions.append(button("删除",async()=>{if(!confirm("确定删除这条帖子吗？帖子将不再展示或参与匹配。"))return;await api("/api/items/"+id,{method:"DELETE"});$("detail-dialog").close();toast("帖子已删除");await loadFeed();}));
  content.append(actions);
 }
 content.append(el("p","认领时请核对未公开的独特细节。AI 不会替你确认归属或自动改变帖子状态。","detail-notice"));
 if(!$("detail-dialog").open)$("detail-dialog").showModal();
}
function formField(name){return $("compose-form").elements.namedItem(name);}
function updateComposeControls(){
 $("publish-button").disabled=state.uploading||state.recognizing||state.saving;
 $("images-input").disabled=state.uploading||state.recognizing||state.images.length>=3;
 $("recognize-button").disabled=state.uploading||state.recognizing||!state.images.length||!formField("allow_ai").checked||!state.config?.ai_enabled;
 formField("allow_ai").disabled=state.recognizing;
 $("image-rule").textContent=(formField("type").value==="found"?"找到帖必须至少上传一张图片。":"丢失帖可以不上传图片。")+" 每帖最多 3 张，单张最大 5 MiB，JPEG / PNG。";
}
function renderImages(){
 $("image-previews").replaceChildren();for(const m of state.images){const box=el("div",undefined,"image-preview");const img=el("img");img.src=m.url;img.alt="待发布物品照片";box.append(img);const remove=button("×",()=>{if(state.recognizing)return;state.images=state.images.filter(x=>x.id!==m.id);renderImages();});remove.setAttribute("aria-label","移除这张照片");box.append(remove);$("image-previews").append(box);}updateComposeControls();
}
function openCompose(item=null){
 if(!state.user){toast("发布前请先登录");$("auth-dialog").showModal();return}
 state.composeVersion++;state.editing=item;state.images=item?[...item.images]:[];state.uploading=false;state.recognizing=false;$("compose-form").reset();
 $("compose-title").textContent=item?"编辑线索":"发布线索";$("publish-button").textContent=item?"保存修改":"发布线索";
 $("compose-message").textContent="";$("recognize-message").textContent=state.config?.ai_enabled?"识图结果需要你检查，尤其是品牌与分类。":"AI 未启用：可以手动填写并发布。";
 if(item){for(const key of ["type","name","location","description","category","color","brand","contact"]){formField(key).value=item[key]||"";}formField("tags").value=item.tags.join(", ");formField("allow_ai").checked=item.allow_ai;
  if(item.occurred_at){const d=new Date(item.occurred_at*1000);formField("occurred_at").value=new Date(d.getTime()-d.getTimezoneOffset()*60000).toISOString().slice(0,16);}
 }renderImages();$("compose-dialog").showModal();
}
async function uploadImages(){
 const files=Array.from($("images-input").files);const version=state.composeVersion;if(state.images.length+files.length>3){toast("每帖最多 3 张图片");$("images-input").value="";return;}
 state.uploading=true;updateComposeControls();$("compose-message").textContent="正在上传并处理图片…";
 try{for(const f of files){if(f.size>5*1024*1024)throw new Error("单张图片最多 5 MiB");const body=new FormData();body.append("image",f);const m=await api("/api/media",{method:"POST",body});if(version!==state.composeVersion)return;state.images.push(m);renderImages();}$("compose-message").textContent="照片已上传，发布前记得检查敏感信息。";}
 catch(e){if(version===state.composeVersion)$("compose-message").textContent=e.message;}
 finally{if(version===state.composeVersion){state.uploading=false;$("images-input").value="";renderImages();}}
}
async function recognize(){
 if(!state.images.length||!formField("allow_ai").checked)return;
 const version=state.composeVersion;state.recognizing=true;updateComposeControls();$("recognize-message").textContent="识图任务已提交，正在等待结果…";
 try{
  const job=await api("/api/ai/extract",{method:"POST",body:{media_id:state.images[0].id,consent:true}});
  for(let attempt=0;attempt<90;attempt++){
   await new Promise(resolve=>setTimeout(resolve,2000));if(version!==state.composeVersion||!$("compose-dialog").open)return;
   const result=await api("/api/ai/jobs/"+job.id);
   if(result.job.status==="failed")throw new Error(result.job.error||"识图失败，可以手动填写。");
   if(result.job.error)$("recognize-message").textContent=result.job.error+"；任务会自动重试，也可以先手动填写。";
   if(result.job.status!=="succeeded")continue;
   const s=result.result;for(const key of ["name","category","color","brand","description"]){if(s[key])formField(key).value=s[key];}if(s.tags?.length)formField("tags").value=s.tags.join(", ");
   $("recognize-message").textContent="已填入识图建议。请核对并补充地点和时间，然后发布。";return;
  }
  throw new Error("任务仍在后台等待。可以手动填写，稍后对同一张照片再次点击识图查询结果。");
 }catch(e){if(version===state.composeVersion)$("recognize-message").textContent=e.message;}
 finally{if(version===state.composeVersion){state.recognizing=false;updateComposeControls();}}
}
async function publish(event){
 event.preventDefault();if(state.uploading||state.recognizing||state.saving)return;
 const body={};for(const key of ["type","name","location","description","category","color","brand","contact"]){body[key]=formField(key).value.trim();}
 body.tags=formField("tags").value.split(/[,，]/).map(x=>x.trim()).filter(Boolean);body.image_ids=state.images.map(m=>m.id);body.allow_ai=formField("allow_ai").checked;
 body.occurred_at=formField("occurred_at").value?Math.floor(new Date(formField("occurred_at").value).getTime()/1000):0;
 if(body.type==="found"&&!body.image_ids.length){$("compose-message").textContent="找到帖至少需要一张图片。";return;}
 state.saving=true;$("publish-button").disabled=true;
 try{await api(state.editing?"/api/items/"+state.editing.id:"/api/items",{method:state.editing?"PUT":"POST",body});$("compose-dialog").close();toast(state.editing?"修改已保存":"线索已发布"+(body.allow_ai?"，匹配将在后台进行":""));state.page=1;await loadFeed();}
 catch(e){$("compose-message").textContent=e.message;}
 finally{state.saving=false;updateComposeControls();}
}
async function loadNotifications(show=false){
 if(!state.user)return;const data=await api("/api/notifications");$("unread").textContent=data.unread?"("+data.unread+")":"";
 if(!show)return;const list=$("notifications-content");list.replaceChildren();
 if(!data.notifications.length)list.append(el("p","暂时没有匹配消息。允许 AI 匹配的帖子会在后台寻找线索。","hint"));
 for(const n of data.notifications){
  const card=el("article",undefined,"notification"+(!n.read_at?" unread":""));card.append(el("h3","可能找到："+n.found_name),el("p","对应丢失帖 #"+n.lost_id+" · 相关度 "+Math.round(n.score*100)+"/100（不是概率）"),el("p",(n.reasons||[]).join("；")),el("p",n.uncertainty||"请核对实物细节，不保证属于同一物品。","hint"));
  if(!n.available)card.append(el("p","帖子已修改、撤回、结束或不再参与 AI；此线索已过时。","hint"));
  const actions=el("div",undefined,"actions");
  const open=button("查看找到帖",async()=>{await api("/api/notifications/"+n.id+"/read",{method:"PATCH"});$("notifications-dialog").close();await openDetail(n.found_id);await loadNotifications();});open.disabled=!n.available;actions.append(open);
  actions.append(button("查看我的丢失帖",async()=>{$("notifications-dialog").close();await openDetail(n.lost_id);}));
  if(!n.read_at)actions.append(button("标为已读",async()=>{await api("/api/notifications/"+n.id+"/read",{method:"PATCH"});await loadNotifications(true);}));
  card.append(actions);list.append(card);
 }
 if(!$("notifications-dialog").open)$("notifications-dialog").showModal();
}
document.querySelectorAll("[data-close]").forEach(b=>b.addEventListener("click",()=>$(b.dataset.close).close()));
$("login-button").addEventListener("click",()=>{$("auth-message").textContent="";$("auth-dialog").showModal();});
$("auth-form").addEventListener("submit",async e=>{
 e.preventDefault();const method=e.submitter?.value||"login";const form=e.currentTarget;const buttons=form.querySelectorAll("button");buttons.forEach(b=>b.disabled=true);
 try{const user=await api("/api/"+method,{method:"POST",body:{username:form.elements.username.value,password:form.elements.password.value}});
  if(method==="register"){$("auth-message").textContent="注册成功，请点击登录。";return;}
  state.user=user;renderAccount();$("auth-dialog").close();form.reset();toast("登录成功");await loadNotifications();
 }catch(error){$("auth-message").textContent=error.message;}finally{buttons.forEach(b=>b.disabled=false);}
});
$("logout-button").addEventListener("click",async()=>{try{await api("/api/logout",{method:"POST"});state.user=null;renderAccount();["detail-dialog","compose-dialog","notifications-dialog"].forEach(id=>$(id).close());toast("已退出登录");}catch(e){toast(e.message);}});
$("new-button").addEventListener("click",()=>openCompose());
$("compose-dialog").addEventListener("close",()=>{state.composeVersion++;state.recognizing=false;});
$("compose-form").addEventListener("submit",publish);formField("type").addEventListener("change",updateComposeControls);formField("allow_ai").addEventListener("change",updateComposeControls);
$("images-input").addEventListener("change",uploadImages);$("recognize-button").addEventListener("click",recognize);
$("notifications-button").addEventListener("click",()=>loadNotifications(true).catch(e=>toast(e.message)));
$("search-form").addEventListener("submit",e=>{e.preventDefault();state.page=1;loadFeed();});
["breadth","status-filter"].forEach(id=>$(id).addEventListener("change",()=>{state.page=1;loadFeed();}));
document.querySelectorAll("[data-type]").forEach(b=>b.addEventListener("click",()=>{state.type=b.dataset.type;state.page=1;document.querySelectorAll("[data-type]").forEach(x=>{x.classList.toggle("active",x===b);x.setAttribute("aria-pressed",String(x===b));});loadFeed();}));
$("previous-page").addEventListener("click",()=>{state.page--;loadFeed();});$("next-page").addEventListener("click",()=>{state.page++;loadFeed();});
async function init(){
 try{state.config=await api("/api/config");$("ai-status").textContent=state.config.ai_enabled?"AI 识图和自动匹配已启用。图片需你授权发送；匹配结果仅作为线索。":"AI 尚未启用，图片发布、手动填表和三档搜索可以正常使用。";}
 catch(e){$("ai-status").textContent=e.message;}
 try{state.user=await api("/api/me");}catch{state.user=null;}
 renderAccount();await loadFeed();if(state.user)await loadNotifications().catch(()=>{});
 const id=Number(new URLSearchParams(location.search).get("item"));if(Number.isInteger(id)&&id>0)await openDetail(id).catch(e=>toast(e.message));
}
init().catch(e=>toast(e.message));setInterval(()=>{if(state.user&&!document.hidden)loadNotifications().catch(()=>{});},30000);
