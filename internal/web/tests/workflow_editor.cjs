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
 if(url.pathname==='/api/github/repos')return route.fulfill({json:{repos:[{full_name:'acme/repo'},{full_name:'acme/other'},{full_name:'person/project',owner_type:'User'}]}});
 if(url.pathname==='/api/status')return route.fulfill({json:{tasks:[],events:[],repositories:[],stats:{},resources:{}}});
 if(url.pathname==='/api/agent-auth')return route.fulfill({json:{ok:true,auth:{required:false,ready:true,state:'ready'}}});
 if(url.pathname==='/api/review-settings')return route.fulfill({json:{ok:true,settings:{workflow_enabled:true}}});
 if(url.pathname.startsWith('/api/'))return route.fulfill({json:{ok:true}});return route.continue()});
 await page.goto(baseURL+'/#workflow');await page.getByRole('heading',{name:'Repository automations'}).waitFor();return {page,getDocument:()=>doc};
}
async function selectRepository(page, repository) {
 const owner = repository.split('/')[0];
 const owners = page.getByRole('group', {name:'Organizations & people',exact:true});
 const button = owners.getByRole('button').filter({hasText:owner});
 if (await button.getAttribute('aria-pressed') !== 'true') await button.click();
 await page.getByRole('group',{name:'Repositories',exact:true}).getByRole('button',{name:repository,exact:true}).click();
}
test('multiple persistent workflows, disabled drafts, policy editing, activation and pause',async t=>{const {page,getDocument}=await openEditor(t);
 await selectRepository(page, 'acme/repo');await page.getByRole('button',{name:'Implement issues',exact:true}).click();await page.locator('.wf-node').first().waitFor();assert.equal(await page.locator('.wf-node').count(),4);
 await page.getByLabel('Workflow name').fill('Implement ready issues');await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows[0].enabled,false);
 await page.getByRole('button',{name:'Enable',exact:true}).click();await page.getByRole('button',{name:'Pause',exact:true}).waitFor();assert.equal(getDocument().workflows[0].enabled,true);await page.getByRole('button',{name:'Pause',exact:true}).click();
 await page.getByRole('button',{name:'Review PRs',exact:true}).click();await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows.length,2);
 await page.getByRole('button',{name:'Preview matches',exact:true}).click();await page.getByText('#12 Example issue',{exact:true}).waitFor();assert.equal(getDocument().workflows.every(w=>!w.enabled),true);
 await page.reload();await selectRepository(page, 'acme/repo');assert.equal(await page.locator('.wf-step').count(),2);await page.locator('.wf-step').first().click();await page.getByLabel('Review step',{exact:true}).check();await page.getByLabel('Maximum repair cycles').fill('3');await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows[0].reviewCycles,3);
 await selectRepository(page, 'acme/other');assert.equal(await page.locator('.wf-step').count(),0);assert.equal(getDocument().workflows.length,2);
 const editor=await page.locator('.wf-editor').boundingBox(),dock=await page.locator('.page-bottom-dock').boundingBox();assert.ok(editor.y+editor.height<=dock.y);});
test('mobile editor exposes repository, canvas and settings without horizontal overflow',async t=>{const {page}=await openEditor(t,{width:390,height:844});await selectRepository(page, 'acme/repo');await page.getByRole('button',{name:'Implement issues',exact:true}).click();await page.getByLabel('Label',{exact:true}).fill('ready');assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));});

test('scheduled template supports library prompts, interval units, and inline fallback',async t=>{const {page,getDocument}=await openEditor(t);await selectRepository(page, 'acme/repo');await page.getByRole('button',{name:'Scheduled task',exact:true}).click();assert.equal(await page.locator('.wf-node').count(),3);await page.getByRole('button',{name:'days',exact:true}).click();await page.locator('[data-id="task"]').click();await page.getByLabel('Prompt source',{exact:true}).selectOption('audit');await page.getByText('Preview library prompt',{exact:true}).click();await page.getByText('Audit the repository.',{exact:true}).waitFor();await page.getByLabel('Additional context (optional)').fill('Focus on security');await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows[0].trigger.event,'schedule');assert.equal(getDocument().workflows[0].task.libraryTaskName,'audit');await page.getByLabel('Prompt source',{exact:true}).selectOption('');await page.getByLabel('Instructions',{exact:true}).fill('Run an inline repository task.');await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows[0].task.libraryTaskName,undefined);});

test('retired automation templates create independent disabled workflows for one repository',async t=>{
 const {page,getDocument}=await openEditor(t);
 await selectRepository(page, 'acme/repo');
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

test('cached owner picker filters repositories and protects unsaved drafts', async t => {
 const {page}=await openEditor(t);
 assert.equal(await page.getByPlaceholder('owner/repository').count(),0);
 assert.equal(await page.getByRole('button',{name:'Implement issues',exact:true}).isDisabled(),true);
 await selectRepository(page,'person/project');
 assert.equal(await page.getByRole('group',{name:'Repositories',exact:true}).getByRole('button').count(),1);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 await page.getByRole('group',{name:'Organizations & people',exact:true}).getByRole('button').filter({hasText:'person'}).click();
 await page.getByText('Save or discard changes first.',{exact:true}).waitFor();
 assert.equal(await page.getByRole('button',{name:'acme/repo',exact:true}).getAttribute('aria-pressed'),'true');
});

test('schedule scale drags between units and preserves minute and month intervals on reload', async t => {
 const {page,getDocument}=await openEditor(t,{width:390,height:844});
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Scheduled task',exact:true}).click();
 const save=async()=>{await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft')};
 await page.getByRole('button',{name:'minutes',exact:true}).click();
 await page.getByLabel('Repeat every (minutes)',{exact:true}).fill('7');
 await save();
 assert.equal(getDocument().workflows[0].trigger.intervalHours,7/60);
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();await page.locator('[data-id="trigger"]').click();
 assert.equal(await page.getByLabel('Repeat every (minutes)',{exact:true}).inputValue(),'7');
 const slider=page.getByRole('slider',{name:'Repeat interval scale'});
 await slider.scrollIntoViewIfNeeded();const box=await slider.boundingBox();
 await page.mouse.move(box.x+8,box.y+box.height/2);await page.mouse.down();await page.mouse.move(box.x+box.width-8,box.y+box.height/2,{steps:12});await page.mouse.up();
 assert.equal(await slider.getAttribute('aria-valuetext'),'months');
 await page.getByLabel('Repeat every (months)',{exact:true}).fill('2');
 await save();assert.equal(getDocument().workflows[0].trigger.intervalHours,1440);
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();await page.locator('[data-id="trigger"]').click();
 assert.equal(await page.getByLabel('Repeat every (months)',{exact:true}).inputValue(),'2');
 await page.getByRole('slider',{name:'Repeat interval scale'}).focus();await page.keyboard.press('Home');await page.keyboard.press('ArrowRight');
 assert.equal(await page.getByRole('slider').getAttribute('aria-valuetext'),'hours');
 await page.getByRole('button',{name:'weeks',exact:true}).click();await save();assert.equal(getDocument().workflows[0].trigger.intervalHours,336);
 const scale=await page.getByRole('slider').boundingBox(),amount=await page.getByLabel('Repeat every (weeks)',{exact:true}).boundingBox();assert.ok(scale.y+scale.height<amount.y);
 assert.equal(await page.locator('.wf-interval-units svg').count(),5);
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
 await page.screenshot({path:'/tmp/agent00-schedule-scale-mobile.png',fullPage:true});
});

test('in-app workflows keep defaults opt-in and compose review and merge steps', async t => {
 const {page,getDocument}=await openEditor(t);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Prompt submission',exact:true}).click();
 assert.equal(await page.locator('.wf-node').count(),3);
 assert.equal(await page.locator('[data-id="trigger"] .lucide-square-pen').count(),1);
 await page.getByLabel('Review step',{exact:true}).check();
 assert.equal(await page.locator('.wf-node').count(),4);
 await page.getByLabel('Maximum repair cycles').fill('2');
 await page.getByLabel('Merge step',{exact:true}).check();
 assert.equal(await page.locator('.wf-node').count(),5);
 await page.getByRole('button',{name:'Save draft',exact:true}).click();
 await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');
 const w=getDocument().workflows[0];
 assert.equal(w.trigger.event,'app.prompt');assert.equal(w.enabled,false);assert.equal(w.reviewCycles,2);assert.equal(w.merge.enabled,true);
 await page.getByLabel('Review step',{exact:true}).uncheck();
 await page.getByLabel('Merge step',{exact:true}).uncheck();
 assert.equal(await page.locator('.wf-node').count(),3);
 await page.locator('[data-id="task"]').click();
 assert.equal(await page.getByLabel('Prompt source',{exact:true}).count(),0);
 await page.getByLabel('Additional instructions (optional)',{exact:true}).fill('Keep the submitted task scope.');
 await page.getByRole('button',{name:'Save draft',exact:true}).click();
 await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');
 await page.getByRole('button',{name:'JSON submission',exact:true}).click();
 assert.equal(await page.locator('[data-id="trigger"] .lucide-file-json').count(),1);
});
