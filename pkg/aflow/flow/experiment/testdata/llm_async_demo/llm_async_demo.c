// SPDX-License-Identifier: GPL-2.0
/*
 * Small fixture driver for the runtime-guided experiment workflow.
 * It is intentionally a semantic test fixture, not a production driver.
 */
#include <linux/completion.h>
#include <linux/delay.h>
#include <linux/fs.h>
#include <linux/miscdevice.h>
#include <linux/module.h>
#include <linux/slab.h>
#include <linux/uaccess.h>
#include <linux/workqueue.h>

#define LLM_ASYNC_SUBMIT _IO('L', 1)
#define LLM_ASYNC_CANCEL _IO('L', 2)
#define LLM_ASYNC_DELAY  _IOW('L', 3, __u32)

static bool buggy_release;
module_param(buggy_release, bool, 0600);
MODULE_PARM_DESC(buggy_release, "emit the release event before callback completion");

struct llm_async_ctx {
	struct work_struct work;
	struct completion callback_done;
	unsigned int generation;
	unsigned int delay_ms;
	bool submitted;
};

static atomic_t next_generation = ATOMIC_INIT(0);

static void llm_event(struct llm_async_ctx *ctx, const char *kind, const char *state)
{
	pr_info("LLMEXP seq=%u object=ctx generation=%u cpu=%u task=%d kind=%s state=%s result=0\n",
		(unsigned int)atomic_inc_return(&next_generation), ctx->generation,
		smp_processor_id(), current->pid, kind, state);
}

static void llm_work(struct work_struct *work)
{
	struct llm_async_ctx *ctx = container_of(work, struct llm_async_ctx, work);

	llm_event(ctx, "callback_enter", "running");
	if (ctx->delay_ms)
		msleep(ctx->delay_ms);
	llm_event(ctx, "callback_exit", "done");
	complete(&ctx->callback_done);
}

static int llm_open(struct inode *inode, struct file *file)
{
	struct llm_async_ctx *ctx;

	ctx = kzalloc(sizeof(*ctx), GFP_KERNEL);
	if (!ctx)
		return -ENOMEM;
	INIT_WORK(&ctx->work, llm_work);
	init_completion(&ctx->callback_done);
	ctx->generation = (unsigned int)atomic_inc_return(&next_generation);
	file->private_data = ctx;
	llm_event(ctx, "create", "created");
	return 0;
}

static long llm_ioctl(struct file *file, unsigned int cmd, unsigned long arg)
{
	struct llm_async_ctx *ctx = file->private_data;
	unsigned int delay;

	if (!ctx)
		return -EINVAL;
	switch (cmd) {
	case LLM_ASYNC_SUBMIT:
		if (!queue_work(system_wq, &ctx->work))
			return -EBUSY;
		ctx->submitted = true;
		llm_event(ctx, "submit", "queued");
		return 0;
	case LLM_ASYNC_CANCEL:
		if (!ctx->submitted)
			return -EINVAL;
		if (!buggy_release)
			cancel_work_sync(&ctx->work);
		llm_event(ctx, "cancel_return", "cancelled");
		return 0;
	case LLM_ASYNC_DELAY:
		if (copy_from_user(&delay, (void __user *)arg, sizeof(delay)))
			return -EFAULT;
		ctx->delay_ms = min(delay, 1000U);
		return 0;
	default:
		return -ENOTTY;
	}
}

static int llm_release(struct inode *inode, struct file *file)
{
	struct llm_async_ctx *ctx = file->private_data;

	if (!ctx)
		return 0;
	if (buggy_release)
		llm_event(ctx, "release", "released-before-callback");
	cancel_work_sync(&ctx->work);
	if (!buggy_release)
		llm_event(ctx, "release", "released");
	kfree(ctx);
	return 0;
}

static const struct file_operations llm_fops = {
	.owner = THIS_MODULE,
	.open = llm_open,
	.release = llm_release,
	.unlocked_ioctl = llm_ioctl,
#ifdef CONFIG_COMPAT
	.compat_ioctl = llm_ioctl,
#endif
};

static struct miscdevice llm_misc = {
	.minor = MISC_DYNAMIC_MINOR,
	.name = "llm_async_demo",
	.fops = &llm_fops,
};

static int __init llm_init(void)
{
	return misc_register(&llm_misc);
}

static void __exit llm_exit(void)
{
	misc_deregister(&llm_misc);
}

module_init(llm_init);
module_exit(llm_exit);
MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("syzkaller runtime-guided async lifecycle fixture");
