const {before,after,test}=require('node:test');
const assert=require('node:assert/strict');
const {chromium}=require('playwright');
const baseURL=process.env.HARNESS_TEST_URL||'http://127.0.0.1:7777';
let browser;
before(async()=>{browser=await chromium.launch({headless:true,args:['--no-sandbox']})});
after(async()=>{await browser?.close()});
async function openEditor(t,viewport={width:1280,height:900}){
 const page=await browser.newPage({viewport});const errors=[];page.on('pageerror',e=>errors.push(e.message));t.after(async()=>{await page.close();assert.deepEqual(errors,[])});
 let doc={version:2,revision:0,workflows:[]};
 await page.addInitScript(()=>{window.EventSource=class{}});
 await page.route('**/*',route=>{const url=new URL(route.request().url());if(url.origin!==new URL(baseURL).origin)return route.abort();
 if(url.pathname==='/api/workflows'){if(route.request().method()==='PUT'){const next=route.request().postDataJSON();if(next.revision!==doc.revision)return route.fulfill({status:409,json:{error:'workflows changed; reload before saving'}});doc={...next,revision:doc.revision+1}}return route.fulfill({json:doc})}
 if(url.pathname==='/api/workflows/preview')return route.fulfill({json:[{item:{number:12,title:'Example issue'},matches:true,reason:'Trigger and filters match'}]});
 if(url.pathname==='/api/library')return route.fulfill({json:{tasks:[{name:'audit',displayName:'Audit repository',prompt:'Audit the repository.'}]}});
 if(url.pathname==='/api/workflows/schedules')return route.fulfill({json:[]});
 if(url.pathname==='/api/workflows/runs')return route.fulfill({json:[]});
 if(url.pathname==='/api/github/repos')return route.fulfill({json:{repos:[{full_name:'acme/repo'},{full_name:'acme/other'}]}});
 if(url.pathname==='/api/status')return route.fulfill({json:{tasks:[],events:[],repositories:[],stats:{},resources:{}}});
 if(url.pathname==='/api/agent-auth')return route.fulfill({json:{ok:true,auth:{required:false,ready:true,state:'ready'}}});
 if(url.pathname==='/api/review-settings')return route.fulfill({json:{ok:true,settings:{workflow_enabled:true}}});
 if(url.pathname.startsWith('/api/'))return route.fulfill({json:{ok:true}});return route.continue()});
 await page.goto(baseURL+'/#workflow');await page.getByRole('heading',{name:'Repository automations'}).waitFor();return {page,getDocument:()=>doc};
}
test('multiple persistent workflows, disabled drafts, policy editing, activation and pause',async t=>{const {page,getDocument}=await openEditor(t);
 await page.getByLabel('Workflow repository').fill('acme/repo');await page.getByRole('button',{name:'Implement issues',exact:true}).click();await page.locator('.wf-node').first().waitFor();assert.equal(await page.locator('.wf-node').count(),6);
 await page.getByLabel('Workflow name').fill('Implement ready issues');await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows[0].enabled,false);
 await page.getByRole('button',{name:'Enable',exact:true}).click();await page.getByRole('button',{name:'Pause',exact:true}).waitFor();assert.equal(getDocument().workflows[0].enabled,true);await page.getByRole('button',{name:'Pause',exact:true}).click();
 await page.getByRole('button',{name:'Review PRs',exact:true}).click();await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows.length,2);
 await page.getByRole('button',{name:'Preview matches',exact:true}).click();await page.getByText('#12 Example issue',{exact:true}).waitFor();assert.equal(getDocument().workflows.every(w=>!w.enabled),true);
 await page.reload();await page.getByLabel('Workflow repository').fill('acme/repo');assert.equal(await page.locator('.wf-step').count(),2);await page.locator('.wf-step').first().click();await page.locator('[data-id="review"]').click();await page.getByLabel('Maximum repair cycles').fill('3');await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows[0].reviewCycles,3);
 await page.getByLabel('Workflow repository').fill('acme/other');assert.equal(await page.locator('.wf-step').count(),0);assert.equal(getDocument().workflows.length,2);
 const editor=await page.locator('.wf-editor').boundingBox(),dock=await page.locator('.page-bottom-dock').boundingBox();assert.ok(editor.y+editor.height<=dock.y);});
test('mobile editor exposes repository, canvas and settings without horizontal overflow',async t=>{const {page}=await openEditor(t,{width:390,height:844});await page.getByLabel('Workflow repository').fill('acme/repo');await page.getByRole('button',{name:'Implement issues',exact:true}).click();await page.getByLabel('Label',{exact:true}).fill('ready');assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));});

test('scheduled template supports library prompts, presets, and inline fallback',async t=>{const {page,getDocument}=await openEditor(t);await page.getByLabel('Workflow repository').fill('acme/repo');await page.getByRole('button',{name:'Scheduled task',exact:true}).click();assert.equal(await page.locator('.wf-node').count(),5);await page.getByRole('button',{name:'Daily',exact:true}).click();await page.locator('[data-id="task"]').click();await page.getByLabel('Prompt source',{exact:true}).selectOption('audit');await page.getByText('Preview library prompt',{exact:true}).click();await page.getByText('Audit the repository.',{exact:true}).waitFor();await page.getByLabel('Additional context (optional)').fill('Focus on security');await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows[0].trigger.event,'schedule');assert.equal(getDocument().workflows[0].task.libraryTaskName,'audit');await page.getByLabel('Prompt source',{exact:true}).selectOption('');await page.getByLabel('Instructions',{exact:true}).fill('Run an inline repository task.');await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows[0].task.libraryTaskName,undefined);});

test('retired automation templates create independent disabled workflows for one repository',async t=>{
 const {page,getDocument}=await openEditor(t);
 await page.getByLabel('Workflow repository').fill('acme/repo');
 await page.getByRole('button',{name:'Assigned issues',exact:true}).click();
 await page.getByRole('button',{name:'Save draft',exact:true}).click();
 await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');
 await page.getByRole('button',{name:'Issue to merge',exact:true}).click();
 await page.getByRole('button',{name:'Save draft',exact:true}).click();
 await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');
 const workflows=getDocument().workflows;
 assert.equal(workflows.length,2);
 assert.ok(workflows.every(w=>w.repository==='acme/repo'&&!w.enabled&&!w.includeBacklog));
 assert.equal(workflows[0].trigger.event,'issue.assigned');
 assert.equal(workflows[0].trigger.value,'@me');
 assert.equal(workflows[0].reviewCycles,2);
 assert.equal(workflows[0].merge.enabled,false);
 assert.deepEqual(workflows[1].merge,{enabled:true,method:'squash',requireApproval:true,deleteBranch:true});
 assert.notEqual(workflows[0].id,workflows[1].id);
});
